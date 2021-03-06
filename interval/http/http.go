package http

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/exmonitor/exclient/database"
	"github.com/exmonitor/exlogger"
	"github.com/pkg/errors"

	"github.com/exmonitor/watcher/interval/spec"
	"github.com/exmonitor/watcher/interval/status"
	"github.com/exmonitor/watcher/key"
	"net/url"
)

const (
	msgFailedToExecute       = "failed to execute http request: "
	msgFailedBadStatusCode   = "failed - bad http status code"
	msgFailedContentNotFound = "failed - content not found"
	msgFailedCertExpired     = "failed - certificate expiration issue"

	msgInternalFailedToReadResponse = "INTERNAL: failed to read http response"
	msgInternalFailedHttpClient     = "INTERNAL: failed to prepare http request"
)

var defaultAllowedStatusCodes = []int{200, 201, 202, 203, 204, 205}

// config is used for initializing the check
type CheckConfig struct {
	// general options
	Id       int
	Interval int
	Port     int
	Target   string // IP or URL
	Timeout  time.Duration

	// protocol specific options
	Proto        string // http or https
	Method       string
	Query        string
	PostData     []HTTPKeyValue
	ExtraHeaders []HTTPKeyValue
	AuthEnabled  bool
	AuthUsername string
	AuthPassword string

	// content specific options
	ContentCheckEnabled bool
	ContentCheckString  string

	// allowed http responses
	AllowedHttpStatusCodes []int

	// https options
	TlsSkipVerify              bool
	TlsCheckCertificates       bool
	TlsCertExpirationThreshold time.Duration

	// db client
	DBClient database.ClientInterface
	Logger   *exlogger.Logger

	// extra info
	FailThreshold int
}

type Check struct {
	// general options
	id        int // id of  the check saved in db, always same for the specific check
	interval  int
	requestId string // identification of this current request, always unique across all data in eternity
	port      int
	target    string // IP or URL
	timeout   time.Duration

	// protocol specific options
	proto        string
	method       string
	query        string
	postData     []HTTPKeyValue
	extraHeaders []HTTPKeyValue
	authEnabled  bool
	authUsername string
	authPassword string

	// content specific options
	contentCheckEnabled bool
	contentCheckString  string

	// allowed http responses status code (ie: [200,404])
	allowedHttpStatusCodes []int

	// https options
	tlsSkipVerify              bool
	tlsCheckCertificates       bool
	tlsCertExpirationThreshold time.Duration

	// db client
	dbClient database.ClientInterface

	// extra
	failThreshold int

	// logger
	log *exlogger.Logger
	// internals
	spec.CheckInterface
}

type HTTPKeyValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func New(conf CheckConfig) (*Check, error) {
	if conf.Id == 0 {
		return nil, errors.Wrap(invalidConfigError, "check.id must not be zero")
	}
	if conf.Interval == 0 {
		return nil, errors.Wrap(invalidConfigError, "check.Interval must not be zero")
	}
	if conf.FailThreshold == 0 {
		return nil, errors.Wrap(invalidConfigError, "check.FailThreshold must not be zero")
	}
	if conf.Port == 0 {
		return nil, errors.Wrap(invalidConfigError, "check.Port must not be zero")
	}
	if conf.Target == "" {
		return nil, errors.Wrap(invalidConfigError, "check.Target must not be empty")
	}
	if conf.Timeout == 0 {
		return nil, errors.Wrap(invalidConfigError, "check.Timeout must not be zero")
	}
	if conf.Method == "" {
		return nil, errors.Wrap(invalidConfigError, "check.Method must not be empty")
	}
	if conf.Method != http.MethodGet && conf.Method != http.MethodHead && conf.Method != http.MethodPost {
		return nil, errors.Wrap(invalidConfigError, "http method "+conf.Method+" is not supported")
	}
	if conf.AuthEnabled && conf.AuthUsername == "" {
		return nil, errors.Wrapf(invalidConfigError, "check.Username must not be empty, when BasicAuth is enabled")
	}
	if len(conf.AllowedHttpStatusCodes) == 0 {
		conf.AllowedHttpStatusCodes = defaultAllowedStatusCodes
	}
	if conf.TlsCheckCertificates && conf.TlsCertExpirationThreshold == 0 {
		return nil, errors.Wrapf(invalidConfigError, "check.tlsCertExpirationThreshold must not be zero, when tlsCheckCertificates is enabled")
	}
	if conf.Logger == nil {
		return nil, errors.Wrapf(invalidConfigError, "check.Logger must not be nil")
	}
	if conf.DBClient == nil {
		return nil, errors.Wrapf(invalidConfigError, "check.DBClient must not be nil")
	}

	// init values
	newCheck := &Check{
		id:       conf.Id,
		interval: conf.Interval,
		port:     conf.Port,
		target:   conf.Target,
		timeout:  conf.Timeout,

		proto:        conf.Proto,
		method:       conf.Method,
		query:        conf.Query,
		extraHeaders: conf.ExtraHeaders,
		authEnabled:  conf.AuthEnabled,
		authUsername: conf.AuthUsername,
		authPassword: conf.AuthPassword,

		contentCheckEnabled: conf.ContentCheckEnabled,
		contentCheckString:  conf.ContentCheckString,

		allowedHttpStatusCodes: conf.AllowedHttpStatusCodes,

		tlsSkipVerify:              conf.TlsSkipVerify,
		tlsCheckCertificates:       conf.TlsCheckCertificates,
		tlsCertExpirationThreshold: conf.TlsCertExpirationThreshold,

		failThreshold: conf.FailThreshold,

		log:      conf.Logger,
		dbClient: conf.DBClient,
	}
	return newCheck, nil
}

// wrapper function used to run in separate thread (goroutine)
func (c *Check) RunCheck() {

	// generate unique request ID
	c.requestId = key.GenerateReqId(c.id)
	// run monitoring check
	s := c.doCheck()
	c.LogResult(s)
	// save result to database
	s.SaveToDB()
}

// run monitoring check with all options
func (c *Check) doCheck() *status.Status {
	statusConfig := status.Config{
		Id:            c.id,
		ReqId:         c.requestId,
		Interval:      c.interval,
		FailThreshold: c.failThreshold,
		DBClient:      c.dbClient,
	}
	s, err := status.New(statusConfig)
	if err != nil {
		c.LogRunError(err, fmt.Sprintf("failed to init new status for HTTP service ID %d", c.id))
	}
	tStart := time.Now()

	// set tls config
	tlsConfig := &tls.Config{
		InsecureSkipVerify: c.tlsSkipVerify,
	}
	// set http transport configuration
	transportConf := &http.Transport{
		ResponseHeaderTimeout: c.timeout,
		IdleConnTimeout:       c.timeout,
		ExpectContinueTimeout: c.timeout,
		TLSHandshakeTimeout:   c.timeout,
		TLSClientConfig:       tlsConfig,
	}
	// initialize http client
	client := http.Client{
		Transport:     transportConf,
		Timeout:       c.timeout,
		CheckRedirect: c.redirectPolicyFunc,
	}
	// prepare http request
	req, err := http.NewRequest(c.method, c.url(), c.getPostData())
	if err != nil {
		c.LogRunError(err, msgInternalFailedHttpClient)
		s.Set(false, err, msgInternalFailedHttpClient)
		return s
	}
	// set basic auth if its enabled
	if c.authEnabled {
		req.SetBasicAuth(c.authUsername, c.authPassword)
	}

	// add all extra http headers
	c.addExtraHeaders(req)

	// execute http request
	resp, err := client.Do(req)
	if err != nil {
		s.Set(false, err, msgFailedToExecute)
		return s
	} else {
		httpCodeOK := false
		// check if http response code is allowed
		for _, allowedStatusCode := range c.allowedHttpStatusCodes {
			if resp.StatusCode == allowedStatusCode {
				httpCodeOK = true
				break
			}
		}
		if !httpCodeOK {
			msg := fmt.Sprintf("HTTP code: %d is in not within allowed codes %s", resp.StatusCode, c.allowedHttpStatusCodes)
			s.Set(false, nil, fmt.Sprintf("%s, %s", msgFailedBadStatusCode, msg))
			return s
		}

		// check for content
		if c.contentCheckEnabled && httpCodeOK {
			// read http response body
			respData, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				c.LogRunError(err, msgInternalFailedToReadResponse)
				s.Set(false, err, msgInternalFailedToReadResponse)
				return s
			}
			// check if response contains requested content in http body
			if !strings.Contains(string(respData), c.contentCheckString) {
				s.Set(false, nil, msgFailedContentNotFound)
				return s
			}
		}
	}
	// check certificates
	if c.tlsCheckCertificates {
		certsOK, message := c.checkTLS(resp.TLS)
		if !certsOK {
			s.Set(false, nil, message)
			return s
		}
	}

	s.Duration = time.Since(tStart)
	s.Set(true, nil, "success")

	return s
}

// redirect policy, in case the target URL is not real page but is redirecting to somewhere else
// we need to re-add all the http headers
func (c *Check) redirectPolicyFunc(req *http.Request, via []*http.Request) error {
	// set basic auth if its enabled
	if c.authEnabled {
		req.SetBasicAuth(c.authUsername, c.authPassword)
	}
	// add all extra http headers
	c.addExtraHeaders(req)

	return nil
}

// add extra http headers to the request
func (c *Check) addExtraHeaders(req *http.Request) {
	// add all extra http headers
	for i := 0; i < len(c.extraHeaders); i++ {
		req.Header.Add(c.extraHeaders[i].Name, c.extraHeaders[i].Value)
	}
}

// add post data
func (c *Check) getPostData() *strings.Reader {
	form := url.Values{}
	for _, item := range c.postData {
		form.Add(item.Name, item.Value)
	}
	return strings.NewReader(form.Encode())

}

func (c *Check) url() string {
	return fmt.Sprintf("%s://%s:%d/%s", c.proto, c.target, c.port, c.query)
}

// check TTL of tls certs
func (c *Check) checkTLS(conn *tls.ConnectionState) (bool, string) {
	certsOK := true
	message := ""
	// check certs
	for _, cert := range conn.PeerCertificates {
		// check if now() + tlsExpirationThreshold > CertExpirationDate
		if time.Now().Add(c.tlsCertExpirationThreshold).After(cert.NotAfter) {
			certsOK = false
			message += fmt.Sprintf("certificate %s will expire in less than %.0f hours", cert.DNSNames, c.tlsCertExpirationThreshold.Hours())
		}
	}

	return certsOK, message
}

func (c *Check) GetStringPort() string {
	return fmt.Sprintf(":%d", c.port)
}

func (c *Check) LogResult(s *status.Status) {
	c.log.Log("check-HTTP|id %d|reqID %s|target %s|proto %s|port %d|latency %sms|result '%t'|msg: %s", c.id, c.requestId, c.target, c.proto, c.port, key.MsFromDuration(s.Duration), s.Result, s.Message)
}

func (c *Check) LogRunError(err error, message string) {
	c.log.LogError(err, "running check id:%d reqID:%s type:http/https target:%s failed, reason: %s", c.id, c.requestId, c.target, message)
}
