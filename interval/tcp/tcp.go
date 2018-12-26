package tcp

import (
	"fmt"
	"net"
	"time"

	"github.com/exmonitor/exclient/database"
	"github.com/exmonitor/exlogger"
	"github.com/exmonitor/watcher/interval/spec"
	"github.com/exmonitor/watcher/interval/status"
	"github.com/exmonitor/watcher/key"
	"github.com/pkg/errors"
)

type CheckConfig struct {
	Id      int
	Target  string
	Port    int
	Timeout time.Duration

	//db client
	DBClient database.ClientInterface
	Logger   *exlogger.Logger
}

type Check struct {
	id        int
	requestId string
	target    string
	port      int
	timeout   time.Duration

	// db client
	dbClient database.ClientInterface
	// logger
	log *exlogger.Logger

	// internals
	spec.CheckInterface
}

func NewCheck(conf CheckConfig) (*Check, error) {
	if conf.Id == 0 {
		return nil, errors.Wrap(invalidConfigError, "conf.Id must not be zero")
	}
	if conf.Target == "" {
		return nil, errors.Wrap(invalidConfigError, "conf.Target must not be empty")
	}
	if conf.Port == 0 {
		return nil, errors.Wrap(invalidConfigError, "conf.Port must not be zero")
	}
	if conf.Timeout == 0 {
		return nil, errors.Wrap(invalidConfigError, "conf.Timeout must not be zero")
	}
	if conf.DBClient == nil {
		return nil, errors.Wrap(invalidConfigError, "conf.DBClient must not be nil")
	}
	if conf.Logger == nil {
		return nil, errors.Wrap(invalidConfigError, "conf.Logger must not be nil")
	}

	newCheck := &Check{
		id:      conf.Id,
		timeout: conf.Timeout,
		port:    conf.Port,
		target:  conf.Target,

		dbClient: conf.DBClient,
		log:      conf.Logger,
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

func (c *Check) doCheck() *status.Status {
	s, err := status.NewStatus(c.dbClient)
	if err != nil {
		c.LogRunError(err, fmt.Sprintf("failed to init new status for ICMP service ID %d", c.id))
	}
	tStart := time.Now()

	conn, err := net.DialTimeout("tcp", tcpTargetAddress(c.target, c.port), c.timeout)
	if err != nil {
		s.Set(false, err, "failed to open tcp connection", "")
		s.Duration = time.Since(tStart)
		return s
	} else {
		defer conn.Close()
		//if _, err := fmt.Fprintf(conn, testMsg); err != nil {
		//	t.Fatal(err)
		//}
		s.Set(true, nil, "success", "")
	}

	s.Duration = time.Since(tStart)
	return s
}

func tcpTargetAddress(target string, port int) string {
	return fmt.Sprintf("%s:%d", target, port)
}

func (c *Check) LogResult(s *status.Status) {
	logMessage := s.Message
	if s.ExtraInfo != "" {
		logMessage += ", ExtraInfo: " + s.ExtraInfo
	}
	if s.Error != nil {
		logMessage += ", Error: " + s.Error.Error()
	}
	c.log.Log("check-TCP|id %d|reqID %s|target %s|port %d|latency %sms|result '%t'|msg: %s", c.id, c.requestId, c.target, c.port, key.MsFromDuration(s.Duration), s.Result, logMessage)
}

func (c *Check) LogRunError(err error, message string) {
	c.log.LogError(err, "running check id:%d reqID:%s type:tcp target:%s%d failed, reason: %s", c.id, c.requestId, c.target, c.port, message)
}