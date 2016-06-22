package chanserv

import (
	"errors"
	"net"
	"time"
)

type server struct {
	mpx Multiplexer

	maxErrMass   int
	onMaxErrMass func(mass int, err error)
	onMpxError   func(err error)
	onError      func(err error)
	onChanError  func(err error)

	timeouts serverTimeouts
}

type serverTimeouts struct {
	servingTimeout    time.Duration
	sourcingTimeout   time.Duration
	chanAcceptTimeout time.Duration

	masterReadTimeout  time.Duration
	masterWriteTimeout time.Duration
	frameReadTimeout   time.Duration
	frameWriteTimeout  time.Duration
}

func NewServer(mpx Multiplexer, opts ...ServerOption) Server {
	srv := server{
		mpx: mpx,

		onError:     func(err error) {},
		onChanError: func(err error) {},
		onMaxErrMass: func(mass int, err error) {
			// TODO: graceful fallback based on the mass value
			time.Sleep(30 * time.Second)
		},

		maxErrMass: 10,
		timeouts: serverTimeouts{
			chanAcceptTimeout: 30 * time.Second,
		},
	}
	for _, o := range opts {
		o(&srv)
	}
	return srv
}

func (s server) ListenAndServe(vAddr string, srcFn SourceFunc) error {
	if s.mpx == nil {
		return errors.New("chanserv: mpx not set")
	}
	l, err := s.mpx.Bind("", vAddr)
	if err != nil {
		return err
	}
	// warn: serve will close listener
	go s.serve(l, srcFn)
	return nil
}

func (s server) serve(listener net.Listener, srcFn SourceFunc) {
	defer listener.Close()

	var errMass int
	for {
		masterConn, err := listener.Accept()
		if err != nil {
			s.onError(err)
			errMass++
			if s.maxErrMass > 0 && errMass >= s.maxErrMass {
				s.onMaxErrMass(errMass, err)
			}
			continue
		}
		errMass = 0
		go s.serveMaster(masterConn, srcFn)
	}
}

func (s server) serveMaster(masterConn net.Conn, srcFn SourceFunc) {
	if s.timeouts.servingTimeout > 0 {
		masterConn.SetDeadline(time.Now().Add(s.timeouts.servingTimeout))
	}
	defer masterConn.Close()

	if d := s.timeouts.masterReadTimeout; d > 0 {
		masterConn.SetReadDeadline(time.Now().Add(d))
	}
	reqBody, err := readFrame(masterConn)
	if err != nil {
		s.onError(err)
		return
	}

	timeout := timerPool.Get().(*time.Timer)
	defer func() {
		timeout.Stop()
		timerPool.Put(timeout)
	}()
	if d := s.timeouts.sourcingTimeout; d > 0 {
		timeout.Reset(d)
	}
	sourceChan := srcFn(reqBody)
	for {
		select {
		case <-timeout.C:
			return
		case out, ok := <-sourceChan:
			if !ok {
				// sourcing is over
				return
			}
			if s.timeouts.sourcingTimeout > 0 {
				timeout.Reset(s.timeouts.sourcingTimeout)
			}
			chanAddr, err := s.bindChannel(out.Out())
			if err != nil {
				s.onError(err)
				continue
			}
			if d := s.timeouts.masterWriteTimeout; d > 0 {
				masterConn.SetWriteDeadline(time.Now().Add(d))
			}
			if err := writeFrame(masterConn, out.Header()); err == nil {
				if err = writeFrame(masterConn, []byte(chanAddr)); err != nil {
					s.onError(err)
				}
			} else {
				s.onError(err)
			}
		}
	}
}

func (s server) bindChannel(out <-chan Frame) (string, error) {
	l, err := s.mpx.Bind("", ":0")
	if err != nil {
		s.onError(err)
		return "", err
	}
	c := channel{
		Listener: l,
		outChan:  out,
		onError:  s.onChanError,
		wTimeout: s.timeouts.frameWriteTimeout,
		aTimeout: s.timeouts.chanAcceptTimeout,
	}
	vAddr := l.Addr().String()
	go c.serve(s.timeouts.servingTimeout)
	return vAddr, nil
}

type channel struct {
	net.Listener

	outChan  <-chan Frame
	wTimeout time.Duration
	aTimeout time.Duration

	onError func(err error)
}

func (c channel) serve(timeout time.Duration) {
	defer c.Close()

	conn, err := acceptTimeout(c, c.aTimeout)
	if err != nil {
		c.onError(err)
		return
	}
	if timeout > 0 {
		conn.SetDeadline(time.Now().Add(timeout))
	}
	defer conn.Close()
	for frame := range c.outChan {
		if c.wTimeout > 0 {
			conn.SetWriteDeadline(time.Now().Add(c.wTimeout))
		}
		if err := writeFrame(conn, frame.Bytes()); err != nil {
			c.onError(err)
		}
	}
}
