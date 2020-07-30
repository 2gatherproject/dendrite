// Copyright 2017 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/matrix-org/dendrite/clientapi/auth/authtypes"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ed25519"
	yaml "gopkg.in/yaml.v2"

	jaegerconfig "github.com/uber/jaeger-client-go/config"
	jaegermetrics "github.com/uber/jaeger-lib/metrics"
)

// Version is the current version of the config format.
// This will change whenever we make breaking changes to the config format.
const Version = 1

// Dendrite contains all the config used by a dendrite process.
// Relative paths are resolved relative to the current working directory
type Dendrite struct {
	// The version of the configuration file.
	// If the version in a file doesn't match the current dendrite config
	// version then we can give a clear error message telling the user
	// to update their config file to the current version.
	// The version of the file should only be different if there has
	// been a breaking change to the config file format.
	Version int `yaml:"version"`

	Global             Global             `yaml:"global"`
	AppServiceAPI      AppServiceAPI      `yaml:"app_service_api"`
	ClientAPI          ClientAPI          `yaml:"client_api"`
	CurrentStateServer CurrentStateServer `yaml:"current_state_server"`
	EDUServer          EDUServer          `yaml:"edu_server"`
	FederationAPI      FederationAPI      `yaml:"federation_api"`
	FederationSender   FederationSender   `yaml:"federation_sender"`
	KeyServer          KeyServer          `yaml:"key_server"`
	MediaAPI           MediaAPI           `yaml:"media_api"`
	RoomServer         RoomServer         `yaml:"room_server"`
	ServerKeyAPI       ServerKeyAPI       `yaml:"server_key_api"`
	SyncAPI            SyncAPI            `yaml:"sync_api"`
	UserAPI            UserAPI            `yaml:"user_api"`

	// The config for tracing the dendrite servers.
	Tracing struct {
		// Set to true to enable tracer hooks. If false, no tracing is set up.
		Enabled bool `yaml:"enabled"`
		// The config for the jaeger opentracing reporter.
		Jaeger jaegerconfig.Configuration `yaml:"jaeger"`
	} `yaml:"tracing"`

	// The config for logging informations. Each hook will be added to logrus.
	Logging []LogrusHook `yaml:"logging"`

	// Any information derived from the configuration options for later use.
	Derived Derived `yaml:"-"`
}

// TODO: Kill Derived
type Derived struct {
	Registration struct {
		// Flows is a slice of flows, which represent one possible way that the client can authenticate a request.
		// http://matrix.org/docs/spec/HEAD/client_server/r0.3.0.html#user-interactive-authentication-api
		// As long as the generated flows only rely on config file options,
		// we can generate them on startup and store them until needed
		Flows []authtypes.Flow `json:"flows"`

		// Params that need to be returned to the client during
		// registration in order to complete registration stages.
		Params map[string]interface{} `json:"params"`
	}

	// Application services parsed from their config files
	// The paths of which were given above in the main config file
	ApplicationServices []ApplicationService

	// Meta-regexes compiled from all exclusive application service
	// Regexes.
	//
	// When a user registers, we check that their username does not match any
	// exclusive application service namespaces
	ExclusiveApplicationServicesUsernameRegexp *regexp.Regexp
	// When a user creates a room alias, we check that it isn't already
	// reserved by an application service
	ExclusiveApplicationServicesAliasRegexp *regexp.Regexp
	// Note: An Exclusive Regex for room ID isn't necessary as we aren't blocking
	// servers from creating RoomIDs in exclusive application service namespaces
}

// KeyPerspectives are used to configure perspective key servers for
// retrieving server keys.
type KeyPerspectives []struct {
	// The server name of the perspective key server
	ServerName gomatrixserverlib.ServerName `yaml:"server_name"`
	// Server keys for the perspective user, used to verify the
	// keys have been signed by the perspective server
	Keys []struct {
		// The key ID, e.g. ed25519:auto
		KeyID gomatrixserverlib.KeyID `yaml:"key_id"`
		// The public key in base64 unpadded format
		PublicKey string `yaml:"public_key"`
	} `yaml:"keys"`
}

// A Path on the filesystem.
type Path string

// A DataSource for opening a postgresql database using lib/pq.
type DataSource string

func (d DataSource) IsSQLite() bool {
	return strings.HasPrefix(string(d), "file:")
}

func (d DataSource) IsPostgres() bool {
	// commented line may not always be true?
	// return strings.HasPrefix(string(d), "postgres:")
	return !d.IsSQLite()
}

// A Topic in kafka.
type Topic string

// An Address to listen on.
type Address string

// FileSizeBytes is a file size in bytes
type FileSizeBytes int64

// ThumbnailSize contains a single thumbnail size configuration
type ThumbnailSize struct {
	// Maximum width of the thumbnail image
	Width int `yaml:"width"`
	// Maximum height of the thumbnail image
	Height int `yaml:"height"`
	// ResizeMethod is one of crop or scale.
	// crop scales to fill the requested dimensions and crops the excess.
	// scale scales to fit the requested dimensions and one dimension may be smaller than requested.
	ResizeMethod string `yaml:"method,omitempty"`
}

// LogrusHook represents a single logrus hook. At this point, only parsing and
// verification of the proper values for type and level are done.
// Validity/integrity checks on the parameters are done when configuring logrus.
type LogrusHook struct {
	// The type of hook, currently only "file" is supported.
	Type string `yaml:"type"`

	// The level of the logs to produce. Will output only this level and above.
	Level string `yaml:"level"`

	// The parameters for this hook.
	Params map[string]interface{} `yaml:"params"`
}

// configErrors stores problems encountered when parsing a config file.
// It implements the error interface.
type configErrors []string

// Load a yaml config file for a server run as multiple processes or as a monolith.
// Checks the config to ensure that it is valid.
func Load(configPath string, monolith bool) (*Dendrite, error) {
	configData, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	basePath, err := filepath.Abs(".")
	if err != nil {
		return nil, err
	}
	// Pass the current working directory and ioutil.ReadFile so that they can
	// be mocked in the tests
	return loadConfig(basePath, configData, ioutil.ReadFile, monolith)
}

func loadConfig(
	basePath string,
	configData []byte,
	readFile func(string) ([]byte, error),
	monolithic bool,
) (*Dendrite, error) {
	var c Dendrite
	c.Defaults()

	var err error
	if err = yaml.Unmarshal(configData, &c); err != nil {
		return nil, err
	}

	if err = c.check(monolithic); err != nil {
		return nil, err
	}

	privateKeyPath := absPath(basePath, c.Global.PrivateKeyPath)
	privateKeyData, err := readFile(privateKeyPath)
	if err != nil {
		return nil, err
	}

	if c.Global.KeyID, c.Global.PrivateKey, err = readKeyPEM(privateKeyPath, privateKeyData); err != nil {
		return nil, err
	}

	for _, certPath := range c.FederationAPI.FederationCertificatePaths {
		absCertPath := absPath(basePath, certPath)
		var pemData []byte
		pemData, err = readFile(absCertPath)
		if err != nil {
			return nil, err
		}
		fingerprint := fingerprintPEM(pemData)
		if fingerprint == nil {
			return nil, fmt.Errorf("no certificate PEM data in %q", absCertPath)
		}
		c.FederationAPI.TLSFingerPrints = append(c.FederationAPI.TLSFingerPrints, *fingerprint)
	}

	c.MediaAPI.AbsBasePath = Path(absPath(basePath, c.MediaAPI.BasePath))

	// Generate data from config options
	err = c.Derive()
	if err != nil {
		return nil, err
	}

	c.Wiring()
	return &c, nil
}

// Derive generates data that is derived from various values provided in
// the config file.
func (config *Dendrite) Derive() error {
	// Determine registrations flows based off config values

	config.Derived.Registration.Params = make(map[string]interface{})

	// TODO: Add email auth type
	// TODO: Add MSISDN auth type

	if config.ClientAPI.RecaptchaEnabled {
		config.Derived.Registration.Params[authtypes.LoginTypeRecaptcha] = map[string]string{"public_key": config.ClientAPI.RecaptchaPublicKey}
		config.Derived.Registration.Flows = append(config.Derived.Registration.Flows,
			authtypes.Flow{Stages: []authtypes.LoginType{authtypes.LoginTypeRecaptcha}})
	} else {
		config.Derived.Registration.Flows = append(config.Derived.Registration.Flows,
			authtypes.Flow{Stages: []authtypes.LoginType{authtypes.LoginTypeDummy}})
	}

	// Load application service configuration files
	if err := loadAppServices(&config.AppServiceAPI, &config.Derived); err != nil {
		return err
	}

	return nil
}

// SetDefaults sets default config values if they are not explicitly set.
func (c *Dendrite) Defaults() {
	c.Version = 1

	c.Global.Defaults()
	c.ClientAPI.Defaults()
	c.CurrentStateServer.Defaults()
	c.EDUServer.Defaults()
	c.FederationAPI.Defaults()
	c.FederationSender.Defaults()
	c.KeyServer.Defaults()
	c.MediaAPI.Defaults()
	c.RoomServer.Defaults()
	c.ServerKeyAPI.Defaults()
	c.SyncAPI.Defaults()
	c.UserAPI.Defaults()
	c.AppServiceAPI.Defaults()

	c.Wiring()
}

func (c *Dendrite) Verify(configErrs *configErrors) {
	type verifiable interface {
		Verify(configErrs *configErrors)
	}
	for _, c := range []verifiable{
		&c.Global, &c.ClientAPI, &c.CurrentStateServer,
		&c.EDUServer, &c.FederationAPI, &c.FederationSender,
		&c.KeyServer, &c.MediaAPI, &c.RoomServer,
		&c.ServerKeyAPI, &c.SyncAPI, &c.UserAPI,
		&c.AppServiceAPI,
	} {
		c.Verify(configErrs)
	}
}

func (c *Dendrite) Wiring() {
	c.ClientAPI.Matrix = &c.Global
	c.CurrentStateServer.Matrix = &c.Global
	c.EDUServer.Matrix = &c.Global
	c.FederationAPI.Matrix = &c.Global
	c.FederationSender.Matrix = &c.Global
	c.KeyServer.Matrix = &c.Global
	c.MediaAPI.Matrix = &c.Global
	c.RoomServer.Matrix = &c.Global
	c.ServerKeyAPI.Matrix = &c.Global
	c.SyncAPI.Matrix = &c.Global
	c.UserAPI.Matrix = &c.Global
	c.AppServiceAPI.Matrix = &c.Global

	c.ClientAPI.Derived = &c.Derived
	c.AppServiceAPI.Derived = &c.Derived
}

// Error returns a string detailing how many errors were contained within a
// configErrors type.
func (errs configErrors) Error() string {
	if len(errs) == 1 {
		return errs[0]
	}
	return fmt.Sprintf(
		"%s (and %d other problems)", errs[0], len(errs)-1,
	)
}

// Add appends an error to the list of errors in this configErrors.
// It is a wrapper to the builtin append and hides pointers from
// the client code.
// This method is safe to use with an uninitialized configErrors because
// if it is nil, it will be properly allocated.
func (errs *configErrors) Add(str string) {
	*errs = append(*errs, str)
}

// checkNotEmpty verifies the given value is not empty in the configuration.
// If it is, adds an error to the list.
func checkNotEmpty(configErrs *configErrors, key, value string) {
	if value == "" {
		configErrs.Add(fmt.Sprintf("missing config key %q", key))
	}
}

// checkNotZero verifies the given value is not zero in the configuration.
// If it is, adds an error to the list.
func checkNotZero(configErrs *configErrors, key string, value int64) {
	if value == 0 {
		configErrs.Add(fmt.Sprintf("missing config key %q", key))
	}
}

// checkPositive verifies the given value is positive (zero included)
// in the configuration. If it is not, adds an error to the list.
func checkPositive(configErrs *configErrors, key string, value int64) {
	if value < 0 {
		configErrs.Add(fmt.Sprintf("invalid value for config key %q: %d", key, value))
	}
}

// checkLogging verifies the parameters logging.* are valid.
func (config *Dendrite) checkLogging(configErrs *configErrors) {
	for _, logrusHook := range config.Logging {
		checkNotEmpty(configErrs, "logging.type", string(logrusHook.Type))
		checkNotEmpty(configErrs, "logging.level", string(logrusHook.Level))
	}
}

// check returns an error type containing all errors found within the config
// file.
func (config *Dendrite) check(_ bool) error { // monolithic
	var configErrs configErrors

	if config.Version != Version {
		configErrs.Add(fmt.Sprintf(
			"unknown config version %q, expected %q", config.Version, Version,
		))
		return configErrs
	}

	config.checkLogging(&configErrs)

	// Due to how Golang manages its interface types, this condition is not redundant.
	// In order to get the proper behaviour, it is necessary to return an explicit nil
	// and not a nil configErrors.
	// This is because the following equalities hold:
	// error(nil) == nil
	// error(configErrors(nil)) != nil
	if configErrs != nil {
		return configErrs
	}
	return nil
}

// absPath returns the absolute path for a given relative or absolute path.
func absPath(dir string, path Path) string {
	if filepath.IsAbs(string(path)) {
		// filepath.Join cleans the path so we should clean the absolute paths as well for consistency.
		return filepath.Clean(string(path))
	}
	return filepath.Join(dir, string(path))
}

func readKeyPEM(path string, data []byte) (gomatrixserverlib.KeyID, ed25519.PrivateKey, error) {
	for {
		var keyBlock *pem.Block
		keyBlock, data = pem.Decode(data)
		if data == nil {
			return "", nil, fmt.Errorf("no matrix private key PEM data in %q", path)
		}
		if keyBlock == nil {
			return "", nil, fmt.Errorf("keyBlock is nil %q", path)
		}
		if keyBlock.Type == "MATRIX PRIVATE KEY" {
			keyID := keyBlock.Headers["Key-ID"]
			if keyID == "" {
				return "", nil, fmt.Errorf("missing key ID in PEM data in %q", path)
			}
			if !strings.HasPrefix(keyID, "ed25519:") {
				return "", nil, fmt.Errorf("key ID %q doesn't start with \"ed25519:\" in %q", keyID, path)
			}
			_, privKey, err := ed25519.GenerateKey(bytes.NewReader(keyBlock.Bytes))
			if err != nil {
				return "", nil, err
			}
			return gomatrixserverlib.KeyID(keyID), privKey, nil
		}
	}
}

func fingerprintPEM(data []byte) *gomatrixserverlib.TLSFingerprint {
	for {
		var certDERBlock *pem.Block
		certDERBlock, data = pem.Decode(data)
		if data == nil {
			return nil
		}
		if certDERBlock.Type == "CERTIFICATE" {
			digest := sha256.Sum256(certDERBlock.Bytes)
			return &gomatrixserverlib.TLSFingerprint{SHA256: digest[:]}
		}
	}
}

// AppServiceURL returns a HTTP URL for where the appservice component is listening.
func (config *Dendrite) AppServiceURL() string {
	// Hard code the appservice server to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.AppServiceAPI.Listen)
}

// RoomServerURL returns an HTTP URL for where the roomserver is listening.
func (config *Dendrite) RoomServerURL() string {
	// Hard code the roomserver to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.RoomServer.Listen)
}

// UserAPIURL returns an HTTP URL for where the userapi is listening.
func (config *Dendrite) UserAPIURL() string {
	// Hard code the userapi to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.UserAPI.Listen)
}

// CurrentStateAPIURL returns an HTTP URL for where the currentstateserver is listening.
func (config *Dendrite) CurrentStateAPIURL() string {
	// Hard code the currentstateserver to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.CurrentStateServer.Listen)
}

// EDUServerURL returns an HTTP URL for where the EDU server is listening.
func (config *Dendrite) EDUServerURL() string {
	// Hard code the EDU server to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.EDUServer.Listen)
}

// FederationSenderURL returns an HTTP URL for where the federation sender is listening.
func (config *Dendrite) FederationSenderURL() string {
	// Hard code the federation sender server to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.FederationSender.Listen)
}

// ServerKeyAPIURL returns an HTTP URL for where the server key API is listening.
func (config *Dendrite) ServerKeyAPIURL() string {
	// Hard code the server key API server to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.ServerKeyAPI.Listen)
}

// KeyServerURL returns an HTTP URL for where the key server is listening.
func (config *Dendrite) KeyServerURL() string {
	// Hard code the key server to talk HTTP for now.
	// If we support HTTPS we need to think of a practical way to do certificate validation.
	// People setting up servers shouldn't need to get a certificate valid for the public
	// internet for an internal API.
	return "http://" + string(config.KeyServer.Listen)
}

// SetupTracing configures the opentracing using the supplied configuration.
func (config *Dendrite) SetupTracing(serviceName string) (closer io.Closer, err error) {
	if !config.Tracing.Enabled {
		return ioutil.NopCloser(bytes.NewReader([]byte{})), nil
	}
	return config.Tracing.Jaeger.InitGlobalTracer(
		serviceName,
		jaegerconfig.Logger(logrusLogger{logrus.StandardLogger()}),
		jaegerconfig.Metrics(jaegermetrics.NullFactory),
	)
}

// logrusLogger is a small wrapper that implements jaeger.Logger using logrus.
type logrusLogger struct {
	l *logrus.Logger
}

func (l logrusLogger) Error(msg string) {
	l.l.Error(msg)
}

func (l logrusLogger) Infof(msg string, args ...interface{}) {
	l.l.Infof(msg, args...)
}
