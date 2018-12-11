package app

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dedis/kyber"
	"github.com/dedis/kyber/suites"
	"github.com/dedis/kyber/util/encoding"
	"github.com/dedis/onet"
	"github.com/dedis/onet/log"
	"github.com/dedis/onet/network"
)

// CothorityConfig is the configuration structure of the cothority daemon.
// - Suite: The cryptographic suite
// - Public: The public key
// - Private: The Private key
// - Address: The external address of the conode, used by others to connect to this one
// - ListenAddress: The address this conode is listening on
// - Description: The description
// - WebSocketTLSCertificate: TLS certificate for the WebSocket
// - WebSocketTLSCertificateKey: TLS certificate key for the WebSocket
type CothorityConfig struct {
	Suite                      string
	Public                     string
	Services                   map[string]ServiceConfig
	Private                    string
	Address                    network.Address
	ListenAddress              string
	Description                string
	WebSocketTLSCertificate    CertificateURL
	WebSocketTLSCertificateKey CertificateURL
}

// ServiceConfig is the configuration of a specific service to override
// default parameters as the key pair
type ServiceConfig struct {
	Public  string
	Private string
}

// Save will save this CothorityConfig to the given file name. It
// will return an error if the file couldn't be created or if
// there is an error in the encoding.
func (hc *CothorityConfig) Save(file string) error {
	fd, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	fd.WriteString("# This file contains your private key.\n")
	fd.WriteString("# Do not give it away lightly!\n")
	err = toml.NewEncoder(fd).Encode(hc)
	if err != nil {
		return err
	}
	return nil
}

// ParseCothority parses the config file into a CothorityConfig.
// It returns the CothorityConfig, the Host so we can already use it, and an error if
// the file is inaccessible or has wrong values in it.
func ParseCothority(file string) (*CothorityConfig, *onet.Server, error) {
	hc := &CothorityConfig{}
	_, err := toml.DecodeFile(file, hc)
	if err != nil {
		return nil, nil, err
	}

	// Backwards compatibility with configs before we included the suite name
	if hc.Suite == "" {
		hc.Suite = "Ed25519"
	}
	suite, err := suites.Find(hc.Suite)
	if err != nil {
		return nil, nil, err
	}

	// Try to decode the Hex values
	private, err := encoding.StringHexToScalar(suite, hc.Private)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing private key: %v", err)
	}
	point, err := encoding.StringHexToPoint(suite, hc.Public)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing public key: %v", err)
	}
	si := network.NewServerIdentity(point, hc.Address)
	si.SetPrivate(private)
	si.Description = hc.Description
	si.ServiceIdentities, err = parseServiceConfig(hc.Services)
	if err != nil {
		return nil, nil, err
	}

	// Same as `NewServerTCP` if `hc.ListenAddress` is empty
	server := onet.NewServerTCPWithListenAddr(si, suite, hc.ListenAddress)

	// Set Websocket TLS if possible
	if hc.WebSocketTLSCertificate != "" && hc.WebSocketTLSCertificateKey != "" {
		tlsCertificate, err := hc.WebSocketTLSCertificate.Content()
		if err != nil {
			return nil, nil, fmt.Errorf("getting WebSocketTLSCertificate content: %v", err)
		}
		tlsCertificateKey, err := hc.WebSocketTLSCertificateKey.Content()
		if err != nil {
			return nil, nil, fmt.Errorf("getting WebSocketTLSCertificateKey content: %v", err)
		}
		cert, err := tls.X509KeyPair(tlsCertificate, tlsCertificateKey)
		if err != nil {
			return nil, nil, fmt.Errorf("loading X509KeyPair: %v", err)
		}

		server.WebSocket.Lock()
		server.WebSocket.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		server.WebSocket.Unlock()
	}
	return hc, server, nil
}

// GroupToml holds the data of the group.toml file.
type GroupToml struct {
	Servers []*ServerToml `toml:"servers"`
}

// NewGroupToml creates a new GroupToml struct from the given ServerTomls.
// Currently used together with calling String() on the GroupToml to output
// a snippet which can be used to create a Cothority.
func NewGroupToml(servers ...*ServerToml) *GroupToml {
	return &GroupToml{
		Servers: servers,
	}
}

// ServerToml is one entry in the group.toml file describing one server to use for
// the cothority.
type ServerToml struct {
	Address     network.Address
	Suite       string
	Public      string
	Description string
	Services    map[string]ServerServiceConfig
	URL         string `toml:"URL,omitempty"`
}

// ServerServiceConfig is a public configuration for a server (i.e. private key
// is missing)
type ServerServiceConfig struct {
	Public string
}

// Group holds the Roster and the server-description.
type Group struct {
	Roster      *onet.Roster
	Description map[*network.ServerIdentity]string
}

// GetDescription returns the description of a ServerIdentity.
func (g *Group) GetDescription(e *network.ServerIdentity) string {
	return g.Description[e]
}

// Save converts the group into a toml structure and save it to the file
func (g *Group) Save(suite suites.Suite, filename string) error {
	servers := make([]*ServerToml, len(g.Roster.List))
	for i, si := range g.Roster.List {
		pub, err := encoding.PointToStringHex(suite, si.Public)
		if err != nil {
			return err
		}

		services := make(map[string]ServerServiceConfig)
		for _, sid := range si.ServiceIdentities {
			suite := onet.ServiceFactory.Suite(sid.Name)

			pub, err := encoding.PointToStringHex(suite, sid.Public)
			if err != nil {
				return err
			}

			services[sid.Name] = ServerServiceConfig{Public: pub}
		}

		servers[i] = &ServerToml{
			Address:     si.Address,
			Suite:       suite.String(),
			Public:      pub,
			Description: si.Description,
			Services:    services,
			URL:         si.URL,
		}
	}

	toml := &GroupToml{Servers: servers}
	return toml.Save(filename)
}

// ReadGroupDescToml reads a group.toml file and returns the list of ServerIdentities
// and descriptions in the file.
// If the file couldn't be decoded or doesn't hold valid ServerIdentities,
// an error is returned.
func ReadGroupDescToml(f io.Reader) (*Group, error) {
	group := &GroupToml{}
	_, err := toml.DecodeReader(f, group)
	if err != nil {
		return nil, err
	}
	// convert from ServerTomls to entities
	var entities = make([]*network.ServerIdentity, len(group.Servers))
	var descs = make(map[*network.ServerIdentity]string)
	for i, s := range group.Servers {
		// Backwards compatibility with old group files.
		if s.Suite == "" {
			s.Suite = "Ed25519"
		}
		en, err := s.toServerIdentity()
		if err != nil {
			return nil, err
		}
		entities[i] = en
		descs[en] = s.Description
	}
	el := onet.NewRoster(entities)
	return &Group{el, descs}, nil
}

// Save writes the GroupToml definition into the file given by its name.
// It will return an error if the file couldn't be created or if writing
// to it failed.
func (gt *GroupToml) Save(fname string) error {
	file, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(gt.String())
	return err
}

// String returns the TOML representation of this GroupToml.
func (gt *GroupToml) String() string {
	var buff bytes.Buffer
	for _, s := range gt.Servers {
		if s.Description == "" {
			s.Description = "Description of your server"
		}
	}
	enc := toml.NewEncoder(&buff)
	if err := enc.Encode(gt); err != nil {
		return "Error encoding grouptoml" + err.Error()
	}
	return buff.String()
}

// toServerIdentity converts this ServerToml struct to a ServerIdentity.
func (s *ServerToml) toServerIdentity() (*network.ServerIdentity, error) {
	suite, err := suites.Find(s.Suite)
	if err != nil {
		return nil, err
	}

	pubR := strings.NewReader(s.Public)
	public, err := encoding.ReadHexPoint(suite, pubR)
	if err != nil {
		return nil, err
	}
	si := network.NewServerIdentity(public, s.Address)
	si.URL = s.URL
	si.Description = s.Description
	si.ServiceIdentities, err = parseServerServiceConfig(s.Services)

	return si, err
}

// NewServerToml takes a public key and an address and returns
// the corresponding ServerToml.
// If an error occurs, it will be printed to StdErr and nil
// is returned.
func NewServerToml(suite network.Suite, public kyber.Point, addr network.Address,
	desc string, services map[string]ServiceConfig) *ServerToml {
	var buff bytes.Buffer
	if err := encoding.WriteHexPoint(suite, &buff, public); err != nil {
		log.Error("Error writing public key")
		return nil
	}

	// Keep only the public key
	publics := make(map[string]ServerServiceConfig)
	for name, conf := range services {
		publics[name] = ServerServiceConfig{Public: conf.Public}
	}

	return &ServerToml{
		Address:     addr,
		Suite:       suite.String(),
		Public:      buff.String(),
		Description: desc,
		Services:    publics,
	}
}

// String returns the TOML representation of the ServerToml.
func (s *ServerToml) String() string {
	var buff bytes.Buffer
	if s.Description == "" {
		s.Description = "## Put your description here for convenience ##"
	}
	enc := toml.NewEncoder(&buff)
	if err := enc.Encode(s); err != nil {
		return "## Error encoding server informations ##" + err.Error()
	}
	return buff.String()
}

// CertificateURLType represents the type of a CertificateURL.
// The supported types are defined as constants of type CertificateURLType.
type CertificateURLType string

// CertificateURL contains the CertificateURLType and the actual URL
// certificate, which can be a path leading to a file containing a certificate
// or a string directly being a certificate.
type CertificateURL string

const (
	// String is a CertificateURL type containing a certificate.
	String CertificateURLType = "string"
	// File is a CertificateURL type that contains the path to a file
	// containing a certificate.
	File = "file"
	// InvalidCertificateURLType is an invalid CertificateURL type.
	InvalidCertificateURLType = "wrong"
	// DefaultCertificateURLType is the default type when no type is specified
	DefaultCertificateURLType = File
)

// typeCertificateURLSep is the separator between the type of the URL
// certificate and the string that identifies the certificate (e.g.
// filepath, content).
const typeCertificateURLSep = "://"

// certificateURLType converts a string to a CertificateURLType. In case of
// failure, it returns InvalidCertificateURLType.
func certificateURLType(t string) CertificateURLType {
	if t == "" {
		return DefaultCertificateURLType
	}
	cuType := CertificateURLType(t)
	types := []CertificateURLType{String, File}
	for _, t := range types {
		if t == cuType {
			return cuType
		}
	}
	return InvalidCertificateURLType
}

// String returns the CertificateURL as a string.
func (cu CertificateURL) String() string {
	return string(cu)
}

// CertificateURLType returns the CertificateURL type from the CertificateURL.
// It returns InvalidCertificateURLType if the CertificateURL is not valid or
// if the CertificateURL type is not known.
func (cu CertificateURL) CertificateURLType() CertificateURLType {
	if !cu.Valid() {
		return InvalidCertificateURLType
	}
	return certificateURLType(cu.typePart())
}

// Valid returns true if the CertificateURL is well formed or false otherwise.
func (cu CertificateURL) Valid() bool {
	vals := strings.Split(string(cu), typeCertificateURLSep)
	if len(vals) > 2 {
		return false
	}
	cuType := certificateURLType(cu.typePart())
	if cuType == InvalidCertificateURLType {
		return false
	}

	return true
}

// Content returns the bytes representing the certificate.
func (cu CertificateURL) Content() ([]byte, error) {
	cuType := cu.CertificateURLType()
	if cuType == String {
		return []byte(cu.blobPart()), nil
	}
	if cuType == File {
		dat, err := ioutil.ReadFile(cu.blobPart())
		if err != nil {
			return nil, err
		}
		return dat, nil
	}
	return nil, fmt.Errorf("Unknown CertificateURL type (%s), cannot get its content", cuType)
}

// typePart returns only the string representing the type of a CertificateURL
// (empty string for no type specified)
func (cu CertificateURL) typePart() string {
	vals := strings.Split(string(cu), typeCertificateURLSep)
	if len(vals) == 1 {
		return ""
	}
	return vals[0]
}

// blobPart returns only the string representing the blob of a CertificateURL
// (the content of the certificate, a file path, ...)
func (cu CertificateURL) blobPart() string {
	vals := strings.Split(string(cu), typeCertificateURLSep)
	if len(vals) == 1 {
		return vals[0]
	}
	return vals[1]
}

// parseServiceConfig takes the map and creates service identities
func parseServiceConfig(configs map[string]ServiceConfig) ([]network.ServiceIdentity, error) {
	si := []network.ServiceIdentity{}

	for name, sc := range configs {
		sid, err := parseServiceIdentity(name, sc.Public, sc.Private)
		if err != nil {
			return nil, err
		}
		si = append(si, *sid)
	}

	return si, nil
}

// parseServerServiceConfig takes the map and creates service identities with only the public key
func parseServerServiceConfig(configs map[string]ServerServiceConfig) ([]network.ServiceIdentity, error) {
	si := []network.ServiceIdentity{}

	for name, sc := range configs {
		sid, err := parseServiceIdentity(name, sc.Public, "")
		if err != nil {
			return nil, err
		}
		si = append(si, *sid)
	}

	return si, nil
}

// parseServiceIdentity creates the service identity
func parseServiceIdentity(name string, pub string, priv string) (*network.ServiceIdentity, error) {
	suite := onet.ServiceFactory.Suite(name)
	if suite == nil {
		return nil, fmt.Errorf("Service `%s` has not been registered with a suite", name)
	}

	private := suite.Scalar()
	var err error
	if priv != "" {
		private, err = encoding.StringHexToScalar(suite, priv)
		if err != nil {
			return nil, fmt.Errorf("parsing `%s` private key: %s", name, err.Error())
		}
	}

	public, err := encoding.StringHexToPoint(suite, pub)
	if err != nil {
		return nil, fmt.Errorf("parsing `%s` public key: %s", name, err.Error())
	}

	si := network.NewServiceIdentity(name, public, private)
	return &si, nil
}
