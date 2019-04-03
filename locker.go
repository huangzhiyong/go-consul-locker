package consulocker

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	api "github.com/hashicorp/consul/api"
)

const (
	// defaultLockRetryInterval how long we wait after a failed lock acquisition
	defaultLockRetryInterval = 10 * time.Second
	// session ttl
	defautSessionTTL = 30 * time.Second
	// if can't acquire, block wait to acquire
	defaultLockWaitTime = 5 * time.Second
	// block min wait time
	minLockWaitTime = 10 * time.Millisecond

	TryAcquireMode = iota
	CallEventModel
)

var (
	defaultLogger = func(tmpl string, s ...interface{}) {}

	ErrKeyNameNull = errors.New("key is null")
	ErrKeyInvalid  = errors.New("Key must not begin with a '/'")
)

// null logger
type loggerType func(tmpl string, s ...interface{})

func SetLogger(logger loggerType) {
	defaultLogger = logger
}

// DisLocker configured for lock acquisition
type DisLocker struct {
	isClosedDoneChan int32
	doneChan         chan struct{}

	consulLock   *api.Lock
	IsLocked     bool
	ConsulClient *api.Client
	Key          string
	SessionID    string

	LockWaitTime      time.Duration
	LockRetryInterval time.Duration
	SessionTTL        time.Duration
}

// Config is used to configure creation of client
type Config struct {
	Address           string // consul addr
	KeyName           string // key on which lock to acquire
	LockWaitTime      time.Duration
	LockRetryInterval time.Duration // interval at which attempt is done to acquire lock
	SessionTTL        time.Duration // time after which consul session will expire and release the lock
}

func (c *Config) check() error {
	if c.KeyName == "" {
		return ErrKeyNameNull
	}

	if strings.HasPrefix(c.KeyName, "/") {
		return ErrKeyInvalid
	}

	return nil
}

func (c *Config) init() {
	if c.LockRetryInterval == 0 {
		c.LockRetryInterval = defaultLockRetryInterval
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = defautSessionTTL
	}
	if c.LockWaitTime == 0 {
		c.LockWaitTime = defaultLockWaitTime
	}
	if c.Address == "" {
		c.Address = "127.0.0.1:8500"
	}
}

func NewConfig() *Config {
	c := &Config{
		LockRetryInterval: defaultLockRetryInterval,
		SessionTTL:        defautSessionTTL,
	}

	return c
}

// New returns a new dislocker object
func New(o *Config) (*DisLocker, error) {
	var (
		locker DisLocker
	)

	// init config
	o.init()

	// set consul server address
	cfg := api.DefaultConfig()
	cfg.Address = o.Address

	// instance consul client, new client share http.conn in DefaultPooledTransport
	consulClient, err := api.NewClient(cfg)
	if err != nil {
		defaultLogger("new consul clinet failed, err: %v", err)
		return &locker, err
	}

	// set
	locker.doneChan = make(chan struct{})
	locker.ConsulClient = consulClient
	locker.Key = o.KeyName
	locker.LockWaitTime = o.LockWaitTime
	locker.LockRetryInterval = o.LockRetryInterval
	locker.SessionTTL = o.SessionTTL

	if err = o.check(); err != nil {
		return &locker, err
	}

	return &locker, nil
}

// RetryLockAcquire attempts to acquire the lock at `LockRetryInterval`
func (d *DisLocker) RetryLockAcquire(value map[string]string, acquired chan<- bool, released chan<- bool, errorChan chan<- error) {
	ticker := time.NewTicker(d.LockRetryInterval)

	for ; true; <-ticker.C {
		value["lock_created_time"] = time.Now().Format(time.RFC3339)
		lock, err := d.acquireLock(d.LockWaitTime, value, CallEventModel, released)
		if err != nil {
			defaultLogger("error on acquireLock :", err, "retry in -", d.LockRetryInterval)
			errorChan <- err
			continue
		}

		if lock {
			defaultLogger("lock acquired with consul session - %s", d.SessionID)
			ticker.Stop()
			acquired <- true
			break
		}
	}
}

func (d *DisLocker) tryLockAcquire(wait time.Duration, value map[string]string) (bool, error) {
	locked, err := d.acquireLock(wait, value, TryAcquireMode, nil)
	if err != nil {
		defaultLogger("acquireLock failed, err: %v", err)
		return locked, err
	}

	if !locked {
		defaultLogger("can't acquire lock, session: %s", d.SessionID)
		return locked, nil
	}

	d.IsLocked = locked
	return locked, nil
}

// TryLockAcquire
func (d *DisLocker) TryLockAcquire(value map[string]string) (bool, error) {
	return d.tryLockAcquire(d.LockWaitTime, value)
}

// TryLockAcquire
func (d *DisLocker) TryLockAcquireNonBlock(value map[string]string) (bool, error) {
	return d.tryLockAcquire(minLockWaitTime, value)
}

// TryLockAcquireBlock
func (d *DisLocker) TryLockAcquireBlock(waitTime time.Duration, value map[string]string) (bool, error) {
	return d.tryLockAcquire(waitTime, value)
}

// ReleaseLock
func (d *DisLocker) ReleaseLock() error {
	if d.SessionID == "" {
		defaultLogger("cannot destroy empty session")
		return nil
	}

	_, err := d.ConsulClient.Session().Destroy(d.SessionID, nil)
	if err != nil {
		return err
	}

	defaultLogger("destroyed consul session: %s", d.SessionID)
	d.IsLocked = false
	if !d.isDoneChanCloed() {
		// only call once
		close(d.doneChan)
	}

	if d.consulLock != nil {
		d.consulLock.Destroy()
	}
	return nil
}

// Renew incr key ttl
func (d *DisLocker) Renew() {
	d.ConsulClient.Session().Renew(d.SessionID, nil)
}

func (d *DisLocker) StartRenewProcess() {
	d.ConsulClient.Session().RenewPeriodic(d.SessionTTL.String(), d.SessionID, nil, d.doneChan)
}

func (d *DisLocker) AsyncStartRenewProcess() {
	go func() {
		d.StartRenewProcess()
	}()
}

func (d *DisLocker) createSession() (string, error) {
	return createSession(d.ConsulClient, d.Key, d.SessionTTL)
}

func (d *DisLocker) recreateSession() error {
	sessionID, err := d.createSession()
	if err != nil {
		return err
	}

	d.SessionID = sessionID
	return nil
}

func (d *DisLocker) isDoneChanCloed() bool {
	select {
	case _, ok := <-d.doneChan:
		if !ok {
			return true
		}
		return false

	default:
		return false
	}
}

func (d *DisLocker) acquireLock(waitTime time.Duration, value map[string]string, mode int, released chan<- bool) (bool, error) {
	if d.SessionID == "" {
		err := d.recreateSession()
		if err != nil {
			return false, err
		}
	}

	if d.isDoneChanCloed() {
		d.doneChan = make(chan struct{})
	}

	b, err := json.Marshal(value)
	if err != nil {
		defaultLogger("error on value marshal", err)
	}

	lock, err := d.ConsulClient.LockOpts(
		&api.LockOptions{
			Key:     d.Key,
			Value:   b,
			Session: d.SessionID,
			// block wait to acquire, consul defualt 15s
			LockWaitTime: waitTime,
			LockTryOnce:  true,
		},
	)
	if err != nil {
		return false, err
	}

	session, _, err := d.ConsulClient.Session().Info(d.SessionID, nil)
	if err == nil && session == nil {
		defaultLogger("consul session: %s is invalid now", d.SessionID)
		d.SessionID = ""
		return false, nil
	}

	if err != nil {
		return false, err
	}

	resp, err := lock.Lock(nil)
	if err != nil {
		return false, err
	}

	if resp == nil {
		return false, nil
	}

	if mode == TryAcquireMode {
		go func() {
			<-resp
			d.IsLocked = false
		}()
	}

	if mode == CallEventModel {
		go func() {
			// wait event
			<-resp
			// close renew process
			if !d.isDoneChanCloed() {
				close(d.doneChan)
			}
			defaultLogger("lock released with session: %s", d.SessionID)
			d.IsLocked = false
			released <- true
		}()

		go d.StartRenewProcess()
	}

	d.consulLock = lock
	return true, nil
}

func createSession(client *api.Client, consulKey string, ttl time.Duration) (string, error) {
	agentChecks, err := client.Agent().Checks()
	if err != nil {
		defaultLogger("error on getting checks, err: %v", err)
		return "", err
	}

	checks := []string{}
	checks = append(checks, "serfHealth")
	for _, j := range agentChecks {
		checks = append(checks, j.CheckID)
	}

	sessionID, _, err := client.Session().Create(
		&api.SessionEntry{
			Name:     consulKey,
			Checks:   checks,
			Behavior: api.SessionBehaviorDelete,
			// after release lock, other get lock wating lockDelay time.
			LockDelay: 1 * time.Microsecond,
			TTL:       ttl.String(),
		},
		nil,
	)
	if err != nil {
		return "", err
	}

	defaultLogger("created consul session: %s", sessionID)
	return sessionID, nil
}
