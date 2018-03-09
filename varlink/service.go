package varlink

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type dispatcher interface {
	VarlinkDispatch(c Call, methodname string) error
	VarlinkGetName() string
	VarlinkGetDescription() string
}

type serviceCall struct {
	Method     string           `json:"method"`
	Parameters *json.RawMessage `json:"parameters,omitempty"`
	More       bool             `json:"more,omitempty"`
	OneShot    bool             `json:"oneshot,omitempty"`
}

type serviceReply struct {
	Parameters interface{} `json:"parameters,omitempty"`
	Continues  bool        `json:"continues,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Service represents an active varlink service. In addition to the registered custom varlink Interfaces, every service
// implements the org.varlink.service interface which allows clients to retrieve information about the
// running service.
type Service struct {
	vendor       string
	product      string
	version      string
	url          string
	interfaces   map[string]dispatcher
	names        []string
	descriptions map[string]string
	running      bool
}

func (s *Service) getInfo(c Call) error {
	return c.replyGetInfo(s.vendor, s.product, s.version, s.url, s.names)
}

func (s *Service) getInterfaceDescription(c Call, name string) error {
	if name == "" {
		return c.ReplyInvalidParameter("interface")
	}

	description, ok := s.descriptions[name]
	if !ok {
		return c.ReplyInterfaceNotFound("interface")
	}

	return c.replyGetInterfaceDescription(description)
}

func (s *Service) handleMessage(writer *bufio.Writer, request []byte) error {
	var in serviceCall

	err := json.Unmarshal(request, &in)

	if err != nil {
		return err
	}

	c := Call{
		writer: writer,
		in:     &in,
	}

	r := strings.LastIndex(in.Method, ".")
	if r <= 0 {
		return c.ReplyInvalidParameter("method")
	}

	interfacename := in.Method[:r]
	methodname := in.Method[r+1:]

	if interfacename == "org.varlink.service" {
		return s.orgvarlinkserviceDispatch(c, methodname)
	}

	// Find the interface and method in our service
	iface, ok := s.interfaces[interfacename]
	if !ok {
		return c.ReplyInterfaceNotFound(interfacename)
	}

	return iface.VarlinkDispatch(c, methodname)
}

func activationListener() net.Listener {
	defer os.Unsetenv("LISTEN_PID")
	defer os.Unsetenv("LISTEN_FDS")

	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil
	}

	nfds, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || nfds != 1 {
		return nil
	}

	syscall.CloseOnExec(3)

	file := os.NewFile(uintptr(3), "LISTEN_FD_3")
	listener, err := net.FileListener(file)
	if err != nil {
		return nil
	}

	return listener
}

// Stop stops a running Service.
func (s *Service) Stop() {
	s.running = false
}

// Run starts a Service.
func (s *Service) Run(address string) error {
	defer func() { s.running = false }()
	s.running = true

	words := strings.SplitN(address, ":", 2)
	protocol := words[0]
	addr := words[1]

	// Ignore parameters after ';'
	words = strings.SplitN(addr, ";", 2)
	if words != nil {
		addr = words[0]
	}

	switch protocol {
	case "unix":
		if addr[0] != '@' {
			os.Remove(addr)
		}

	case "tcp":
		break

	default:
		return fmt.Errorf("Unknown protocol")
	}

	l := activationListener()
	if l == nil {
		var err error
		l, err = net.Listen(protocol, addr)
		if err != nil {
			return err
		}
	}

	defer l.Close()

	handleConnection := func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)

		for s.running {
			request, err := reader.ReadBytes('\x00')
			if err != nil {
				break
			}

			err = s.handleMessage(writer, request[:len(request)-1])
			if err != nil {
				break
			}
		}

		conn.Close()
		if !s.running {
			l.Close()
		}
	}

	for s.running {
		conn, err := l.Accept()
		if err != nil && s.running {
			return err
		}

		go handleConnection(conn)
	}

	return nil
}

// RegisterInterface registers a varlink.Interface containing struct to the Service
func (s *Service) RegisterInterface(iface dispatcher) error {
	name := iface.VarlinkGetName()
	if _, ok := s.interfaces[name]; ok {
		return fmt.Errorf("interface '%s' already registered", name)
	}

	if s.running {
		return fmt.Errorf("service is already running")
	}
	s.interfaces[name] = iface
	s.descriptions[name] = iface.VarlinkGetDescription()
	s.names = append(s.names, name)

	return nil
}

// NewService creates a new Service which implements the list of given varlink interfaces.
func NewService(vendor string, product string, version string, url string) *Service {
	s := Service{
		vendor:       vendor,
		product:      product,
		version:      version,
		url:          url,
		interfaces:   make(map[string]dispatcher),
		descriptions: make(map[string]string),
	}
	s.RegisterInterface(orgvarlinkserviceNew())

	return &s
}
