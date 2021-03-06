package tcpint

import (
	"bufio"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"
)

const (
	NULLBYTE byte = 0
)

type Proxy struct {
	from          string
	to            string
	done          chan struct{}
	log           *log.Entry
	clienthandler func([]byte) []byte
	remotehandler func([]byte) []byte
	delimeter     byte

	// dynamic fields
	clientinjector []byte
	remoteinjector []byte

	sync.Mutex
}

type SafeConn struct {
	net.Conn

	sync.Mutex
}

func NewProxy(from, to string, clienthandler, remotehandler func([]byte) []byte, delimeter byte) *Proxy {
	return &Proxy{
		from: from,
		to:   to,
		done: make(chan struct{}),
		log: log.WithFields(log.Fields{
			"from": from,
			"to":   to,
		}),
		clienthandler:  clienthandler,
		remotehandler:  remotehandler,
		clientinjector: []byte{},
		remoteinjector: []byte{},
		delimeter:      delimeter,
	}
}

func NewSafeConn(conn net.Conn) *SafeConn {
	return &SafeConn{
		Conn: conn,
	}
}

// Readers
func (p *Proxy) Stopped() bool {
	if p.done != nil {
		return false
	}
	return true
}

// Writers
func (p *Proxy) Inject(writertype string, b []byte) {
	var r []byte

	if len(b) > 0 {
		switch writertype {
		case "remote":
			r = append(p.remoteinjector, b...)
			p.remoteinjector = r
		default: // "client"
			r = append(p.clientinjector, b...)
			p.clientinjector = r
		}
	}
}

func (p *Proxy) ClearInject(writertype string) {
	p.Lock()
	defer p.Unlock()

	switch writertype {
	case "remote":
		p.remoteinjector = nil
	default: // "client"
		p.clientinjector = nil
	}
}

// Start proxy server
func (p *Proxy) Start() error {
	p.log.Infoln("Starting proxy on", p.from)
	listener, err := net.Listen("tcp", p.from)
	if err != nil {
		return err
	}
	go p.run(listener)
	return nil
}

// Stop proxy server
func (p *Proxy) Stop() {
	// Close channel
	if p.done == nil {
		return
	}
	p.log.Infoln("Stopping proxy")
	close(p.done)
	p.done = nil
}

func (p *Proxy) run(listener net.Listener) {
	for {
		select {
		// If our proxy is stopped, return
		case <-p.done:
			return
		default:
			connection, err := listener.Accept()
			if err == nil {
				p.log.Infoln("New connection")
				go p.handle(connection)
			} else {
				p.log.WithField("err", err).Errorln("Error accepting conn")
			}
		}
	}
}

func (p *Proxy) handle(connection net.Conn) {
	// New incoming connection from a client
	p.log.Debugln("Handling", connection)
	defer p.log.Debugln("Done handling", connection)
	defer connection.Close()
	// Connect to remote server
	remote, err := net.Dial("tcp", p.to)
	if err != nil {
		p.log.WithField("err", err).Errorln("Error dialing remote host")
		return
	}
	defer remote.Close()
	// Wrap net.Conn in SafeConn to provide mutex support
	safeconnection := NewSafeConn(connection)
	saferemote := NewSafeConn(remote)
	// Create a new waitgroup
	wg := &sync.WaitGroup{}
	wg.Add(2)
	// Pushing data from client to remote host
	go p.intercept(safeconnection, saferemote, "client", "remote", wg)
	// Pushing data to client from remote host
	go p.intercept(saferemote, safeconnection, "remote", "client", wg)
	wg.Wait()
}

func (p *Proxy) processinjection(to *SafeConn, writertype string) {
	select {
	case <-p.done:
		return
	default:
		for {
			var err error
			var injector []byte

			// Get injected bytes
			p.Lock()
			switch writertype {
			case "remote":
				injector = p.remoteinjector
			default: // "client"
				injector = p.clientinjector
			}
			p.Unlock()

			if len(injector) > 0 {
				// Read injected bytes
				p.log.WithField("data", injector).Infoln("Found injected bytes")
				injectbuf := make([]byte, len(injector))
				_ = copy(injectbuf, injector)
				p.ClearInject(writertype)

				// Write injected bytes
				to.Lock()
				p.log.WithField("data", injectbuf).Infoln("Writing injected bytes")
				_, err = to.Write(injectbuf)
				if err != nil {
					to.Unlock()
					p.log.WithField("err", err).Errorln("Error writing injected bytes")
					p.Stop()
					return
				}
				to.Unlock()
			}
		}
	}
}

// fn func([]byte) []byte, injector []byte
func (p *Proxy) intercept(from, to *SafeConn, readertype string, writertype string, wg *sync.WaitGroup) {
	defer wg.Done()
	// Create reader
	r := bufio.NewReader(from)

	// Set parameters
	var fn func([]byte) []byte
	switch readertype {
	case "remote":
		fn = p.remotehandler
	default: // "client"
		fn = p.clienthandler
	}

	// Start injection loop
	go p.processinjection(to, writertype)

	select {
	// If our proxy is stopped, return
	case <-p.done:
		return
	default:
		for {
			var buf []byte
			var err error

			// Read bytes up to delimeter
			buf, err = r.ReadBytes(p.delimeter)
			if err != nil {
				p.log.WithField("err", err).Errorln("Error from reader")
				p.Stop()
				return
			}

			// Run process function
			to.Lock()

			modbuf := fn(buf)

			if len(modbuf) > 0 {
				// Write bytes to other side
				_, err = to.Write(modbuf)
				if err != nil {
					to.Unlock()
					p.log.WithField("err", err).Errorln("Error writing")
					p.Stop()
					return
				}
			}

			to.Unlock()
		}
	}
}
