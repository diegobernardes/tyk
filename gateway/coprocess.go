package gateway

import (
	"C"
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sirupsen/logrus"

	"github.com/TykTechnologies/tyk/apidef"
	"github.com/TykTechnologies/tyk/config"
	"github.com/TykTechnologies/tyk/coprocess"

	"errors"
	"io/ioutil"
	"net/http"
)
import "fmt"

var (
	supportedDrivers = []apidef.MiddlewareDriver{apidef.PythonDriver, apidef.LuaDriver, apidef.GrpcDriver}
	loadedDrivers    = map[apidef.MiddlewareDriver]coprocess.Dispatcher{}
)

// CoProcessMiddleware is the basic CP middleware struct.
type CoProcessMiddleware struct {
	BaseMiddleware
	HookType         coprocess.HookType
	HookName         string
	MiddlewareDriver apidef.MiddlewareDriver
	RawBodyOnly      bool

	successHandler *SuccessHandler
}

func (m *CoProcessMiddleware) Name() string {
	return "CoProcessMiddleware"
}

// CreateCoProcessMiddleware initializes a new CP middleware, takes hook type (pre, post, etc.), hook name ("my_hook") and driver ("python").
func CreateCoProcessMiddleware(hookName string, hookType coprocess.HookType, mwDriver apidef.MiddlewareDriver, baseMid BaseMiddleware) func(http.Handler) http.Handler {
	dMiddleware := &CoProcessMiddleware{
		BaseMiddleware:   baseMid,
		HookType:         hookType,
		HookName:         hookName,
		MiddlewareDriver: mwDriver,
		successHandler:   &SuccessHandler{baseMid},
	}

	return createMiddleware(dMiddleware)
}

func DoCoprocessReload() {
	log.WithFields(logrus.Fields{
		"prefix": "coprocess",
	}).Info("Reloading middlewares")
	if dispatcher := loadedDrivers[apidef.PythonDriver]; dispatcher != nil {
		dispatcher.Reload()
	}
}

// CoProcessor represents a CoProcess during the request.
type CoProcessor struct {
	HookType   coprocess.HookType
	Middleware *CoProcessMiddleware
}

// ObjectFromRequest constructs a CoProcessObject from a given http.Request.
func (c *CoProcessor) ObjectFromRequest(r *http.Request) (*coprocess.Object, error) {
	headers := ProtoMap(r.Header)

	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	if host != "" {
		headers["Host"] = host
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	miniRequestObject := &coprocess.MiniRequestObject{
		Headers:        headers,
		SetHeaders:     map[string]string{},
		DeleteHeaders:  []string{},
		Url:            r.URL.String(),
		Params:         ProtoMap(r.URL.Query()),
		AddParams:      map[string]string{},
		ExtendedParams: ProtoMap(nil),
		DeleteParams:   []string{},
		ReturnOverrides: &coprocess.ReturnOverrides{
			ResponseCode: -1,
		},
		Method:     r.Method,
		RequestUri: r.RequestURI,
		Scheme:     scheme,
	}

	if r.Body != nil {
		defer r.Body.Close()
		var err error
		miniRequestObject.RawBody, err = ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if utf8.Valid(miniRequestObject.RawBody) && !c.Middleware.RawBodyOnly {
			miniRequestObject.Body = string(miniRequestObject.RawBody)
		}
	}

	object := &coprocess.Object{
		Request:  miniRequestObject,
		HookName: c.Middleware.HookName,
	}

	// If a middleware is set, take its HookType, otherwise override it with CoProcessor.HookType
	if c.Middleware != nil && c.HookType == 0 {
		c.HookType = c.Middleware.HookType
	}

	object.HookType = c.HookType

	object.Spec = make(map[string]string)

	// Append spec data:
	if c.Middleware != nil {
		configDataAsJSON := []byte("{}")
		if len(c.Middleware.Spec.ConfigData) > 0 {
			var err error
			configDataAsJSON, err = json.Marshal(c.Middleware.Spec.ConfigData)
			if err != nil {
				return nil, err
			}
		}

		object.Spec = map[string]string{
			"OrgID":       c.Middleware.Spec.OrgID,
			"APIID":       c.Middleware.Spec.APIID,
			"config_data": string(configDataAsJSON),
		}
	}

	// Encode the session object (if not a pre-process & not a custom key check):
	if c.HookType != coprocess.HookType_Pre && c.HookType != coprocess.HookType_CustomKeyCheck {
		if session := ctxGetSession(r); session != nil {
			object.Session = ProtoSessionState(session)
			// For compatibility purposes:
			object.Metadata = object.Session.Metadata
		}
	}

	return object, nil
}

// ObjectPostProcess does CoProcessObject post-processing (adding/removing headers or params, etc.).
func (c *CoProcessor) ObjectPostProcess(object *coprocess.Object, r *http.Request) {
	r.ContentLength = int64(len(object.Request.RawBody))
	r.Body = ioutil.NopCloser(bytes.NewReader(object.Request.RawBody))

	for _, dh := range object.Request.DeleteHeaders {
		r.Header.Del(dh)
	}

	for h, v := range object.Request.SetHeaders {
		r.Header.Set(h, v)
	}

	values := r.URL.Query()
	for _, k := range object.Request.DeleteParams {
		values.Del(k)
	}

	for p, v := range object.Request.AddParams {
		values.Set(p, v)
	}

	r.URL, _ = url.ParseRequestURI(object.Request.Url)
	r.URL.RawQuery = values.Encode()
}

// CoProcessInit creates a new CoProcessDispatcher, it will be called when Tyk starts.
func CoProcessInit() {
	if !config.Global().CoProcessOptions.EnableCoProcess {
		log.WithFields(logrus.Fields{
			"prefix": "coprocess",
		}).Info("Rich plugins are disabled")
		return
	}

	// Load gRPC dispatcher:
	if config.Global().CoProcessOptions.CoProcessGRPCServer != "" {
		var err error
		loadedDrivers[apidef.GrpcDriver], err = NewGRPCDispatcher()
		if err == nil {
			log.WithFields(logrus.Fields{
				"prefix": "coprocess",
			}).Info("gRPC dispatcher was initialized")
		} else {
			log.WithFields(logrus.Fields{
				"prefix": "coprocess",
			}).WithError(err).Error("Couldn't load gRPC dispatcher")
		}
	}
}

// EnabledForSpec checks if this middleware should be enabled for a given API.
func (m *CoProcessMiddleware) EnabledForSpec() bool {
	if !config.Global().CoProcessOptions.EnableCoProcess {
		log.WithFields(logrus.Fields{
			"prefix": "coprocess",
		}).Error("Your API specifies a CP custom middleware, either Tyk wasn't build with CP support or CP is not enabled in your Tyk configuration file!")
		return false
	}

	var supported bool
	for _, driver := range supportedDrivers {
		if m.Spec.CustomMiddleware.Driver == driver {
			supported = true
		}
	}

	if !supported {
		log.WithFields(logrus.Fields{
			"prefix": "coprocess",
		}).Errorf("Unsupported driver '%s'", m.Spec.CustomMiddleware.Driver)
		return false
	}

	if d, _ := loadedDrivers[m.Spec.CustomMiddleware.Driver]; d == nil {
		log.WithFields(logrus.Fields{
			"prefix": "coprocess",
		}).Errorf("Driver '%s' isn't loaded", m.Spec.CustomMiddleware.Driver)
		return false
	}

	log.WithFields(logrus.Fields{
		"prefix": "coprocess",
	}).Debug("Enabling CP middleware.")
	m.successHandler = &SuccessHandler{m.BaseMiddleware}
	return true
}

// ProcessRequest will run any checks on the request on the way through the system, return an error to have the chain fail
func (m *CoProcessMiddleware) ProcessRequest(w http.ResponseWriter, r *http.Request, _ interface{}) (error, int) {
	logger := m.Logger()
	logger.Debug("CoProcess Request, HookType: ", m.HookType)

	var extractor IdExtractor
	if m.Spec.EnableCoProcessAuth && m.Spec.CustomMiddleware.IdExtractor.Extractor != nil {
		extractor = m.Spec.CustomMiddleware.IdExtractor.Extractor.(IdExtractor)
	}

	var returnOverrides ReturnOverrides
	var sessionID string

	if m.HookType == coprocess.HookType_CustomKeyCheck && extractor != nil {
		sessionID, returnOverrides = extractor.ExtractAndCheck(r)

		if returnOverrides.ResponseCode != 0 {
			if returnOverrides.ResponseError == "" {
				return nil, returnOverrides.ResponseCode
			}
			err := errors.New(returnOverrides.ResponseError)
			return err, returnOverrides.ResponseCode
		}
	}

	// It's also possible to override the HookType:
	coProcessor := CoProcessor{
		Middleware: m,
		// HookType: coprocess.PreHook,
	}

	object, err := coProcessor.ObjectFromRequest(r)
	if err != nil {
		logger.WithError(err).Error("Failed to build request object")
		return errors.New("Middleware error"), 500
	}

	t1 := time.Now()
	returnObject, err := coProcessor.Dispatch(object)
	t2 := time.Now()

	if err != nil {
		logger.WithError(err).Error("Dispatch error")
		if m.HookType == coprocess.HookType_CustomKeyCheck {
			return errors.New("Key not authorised"), 403
		} else {
			return errors.New("Middleware error"), 500
		}
	}

	ms := float64(t2.UnixNano()-t1.UnixNano()) * 0.000001
	m.logger.WithField("ms", ms).Debug("gRPC request processing took")

	coProcessor.ObjectPostProcess(returnObject, r)

	var token string
	if returnObject.Session != nil {
		// For compatibility purposes, inject coprocess.Object.Metadata fields:
		if returnObject.Metadata != nil {
			if returnObject.Session.Metadata == nil {
				returnObject.Session.Metadata = make(map[string]string)
			}
			for k, v := range returnObject.Metadata {
				returnObject.Session.Metadata[k] = v
			}
		}

		token = returnObject.Session.Metadata["token"]
	}

	// The CP middleware indicates this is a bad auth:
	if returnObject.Request.ReturnOverrides.ResponseCode > 400 {
		logger.WithField("key", obfuscateKey(token)).Info("Attempted access with invalid key")

		for h, v := range returnObject.Request.ReturnOverrides.Headers {
			w.Header().Set(h, v)
		}

		// Fire Authfailed Event
		AuthFailed(m, r, token)

		// Report in health check
		reportHealthValue(m.Spec, KeyFailure, "1")

		errorMsg := "Key not authorised"
		if returnObject.Request.ReturnOverrides.ResponseError != "" {
			errorMsg = returnObject.Request.ReturnOverrides.ResponseError
		}

		return errors.New(errorMsg), int(returnObject.Request.ReturnOverrides.ResponseCode)
	}

	if returnObject.Request.ReturnOverrides.ResponseCode > 0 {
		for h, v := range returnObject.Request.ReturnOverrides.Headers {
			w.Header().Set(h, v)
		}
		w.WriteHeader(int(returnObject.Request.ReturnOverrides.ResponseCode))
		w.Write([]byte(returnObject.Request.ReturnOverrides.ResponseError))

		// Record analytics data:
		res := new(http.Response)
		res.Proto = "HTTP/1.0"
		res.ProtoMajor = 1
		res.ProtoMinor = 0
		res.StatusCode = int(returnObject.Request.ReturnOverrides.ResponseCode)
		res.Body = nopCloser{
			ReadSeeker: strings.NewReader(returnObject.Request.ReturnOverrides.ResponseError),
		}
		res.ContentLength = int64(len(returnObject.Request.ReturnOverrides.ResponseError))
		m.successHandler.RecordHit(r, int64(ms), int(returnObject.Request.ReturnOverrides.ResponseCode), res)
		return nil, mwStatusRespond
	}

	// Is this a CP authentication middleware?
	if m.Spec.EnableCoProcessAuth && m.HookType == coprocess.HookType_CustomKeyCheck {
		// The CP middleware didn't setup a session:
		if returnObject.Session == nil || token == "" {
			authHeaderValue := r.Header.Get(m.Spec.Auth.AuthHeaderName)
			AuthFailed(m, r, authHeaderValue)
			return errors.New("Key not authorised"), 403
		}

		returnedSession := TykSessionState(returnObject.Session)

		// If the returned object contains metadata, add them to the session:
		for k, v := range returnObject.Metadata {
			returnedSession.MetaData[k] = string(v)
		}

		if extractor == nil {
			ctxSetSession(r, returnedSession, token, true)
		} else {
			ctxSetSession(r, returnedSession, sessionID, true)
		}
	}

	return nil, 200
}

func (c *CoProcessor) Dispatch(object *coprocess.Object) (*coprocess.Object, error) {
	dispatcher := loadedDrivers[c.Middleware.MiddlewareDriver]
	if dispatcher == nil {
		err := fmt.Errorf("Couldn't dispatch request, driver '%s' isn't available", c.Middleware.MiddlewareDriver)
		return nil, err
	}
	newObject, err := dispatcher.Dispatch(object)
	if err != nil {
		return nil, err
	}
	return newObject, nil
}