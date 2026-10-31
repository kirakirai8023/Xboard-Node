// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kirakirai8023/Xboard-Node/internal/cert/dnsproviders"
	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
	"github.com/kirakirai8023/Xboard-Node/internal/panel"
	"gopkg.in/yaml.v3"
)

type deploymentNode struct {
	ID        int    `json:"id"`
	Protocol  string `json:"protocol"`
	Network   string `json:"network"`
	TLS       int    `json:"tls"`
	Port      int    `json:"port"`
	Listen    string `json:"listen"`
	Domain    string `json:"domain"`
	Path      string `json:"path"`
	CertMode  string `json:"cert_mode"`
	PanelCert bool   `json:"panel_cert"`
	Users     int    `json:"users"`
	NeedsCert bool   `json:"needs_cert"`
}

type deploymentReport struct {
	Nodes    []deploymentNode `json:"nodes"`
	Warnings []string         `json:"warnings"`
}

func runDeployment(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: xbctl deploy <inspect|cert> [flags]")
	}
	switch args[0] {
	case "inspect":
		return runDeploymentInspect(args[1:])
	case "cert":
		return runDeploymentCert(args[1:])
	default:
		return errors.New("unknown deployment command")
	}
}

func readDeploymentSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("could not read credential file")
	}
	value := strings.TrimRight(string(data), "\r\n")
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("credential file must contain one nonempty line")
	}
	return value, nil
}

func runDeploymentInspect(args []string) error {
	fs := flag.NewFlagSet("deploy inspect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	panelURL := fs.String("panel-url", "", "panel URL")
	tokenFile := fs.String("token-file", "", "credential file")
	mode := fs.String("mode", "node", "node or machine")
	nodeID := fs.Int("node-id", 0, "node ID")
	machineID := fs.Int("machine-id", 0, "machine ID")
	nodeType := fs.String("node-type", "vless", "node type")
	kernelType := fs.String("kernel", "xray", "kernel")
	topology := fs.String("topology", "auto", "auto, direct, cloudflare, proxy, or cdn")
	output := fs.String("output", "text", "text, json or tsv")
	configPath := fs.String("config", "", "installed configuration")
	existingPath := fs.String("existing-config", "", "reuse matching instance certificate")
	credentialsPath := fs.String("credentials-in", defaultCredentialsPath, "installed credentials")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("invalid deploy inspect arguments")
	}
	if *topology != "auto" && *topology != "direct" && *topology != "cloudflare" && *topology != "proxy" && *topology != "cdn" {
		return errors.New("unsupported deployment topology")
	}
	var configurations []config.Config
	if *configPath != "" {
		restore, err := deploymentCredentials(*credentialsPath)
		if err != nil {
			return err
		}
		defer restore()
		root, err := config.LoadRoot(*configPath)
		if err != nil {
			return errors.New("could not load installed configuration; check syntax and credentials")
		}
		if len(root.Instances) == 0 {
			configurations = []config.Config{root.Config}
		} else {
			configurations = root.Instances
		}
	} else {
		u, err := url.Parse(*panelURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("a valid panel URL without credentials or query is required")
		}
		token, err := readDeploymentSecret(*tokenFile)
		if err != nil {
			return err
		}
		cfg := config.Config{Panel: config.PanelConfig{URL: *panelURL, Token: token, NodeID: *nodeID, NodeType: *nodeType}, Kernel: config.KernelConfig{Type: *kernelType}}
		if *mode == "machine" && *machineID > 0 {
			cfg.Machine = &config.MachineConfig{MachineID: *machineID, Token: token}
		} else if *mode != "node" || *nodeID <= 0 {
			return errors.New("provide a positive node ID, or machine mode with a positive machine ID")
		}
		if *existingPath != "" {
			restore, err := deploymentCredentials(*credentialsPath)
			if err != nil {
				return err
			}
			defer restore()
			root, err := config.LoadRoot(*existingPath)
			if err != nil {
				return errors.New("could not read existing configuration; repair it before adding an instance")
			}
			id, err := cfg.AutoInstanceID()
			if err != nil {
				return errors.New("could not identify the instance")
			}
			for _, existing := range normalizeRootInstances(root) {
				previous, err := existing.AutoInstanceID()
				if err == nil && previous == id {
					cfg.Cert = existing.Cert
					break
				}
			}
		}
		configurations = []config.Config{cfg}
	}
	report, err := inspectDeployment(configurations, *topology)
	if err != nil {
		return err
	}
	if *configPath != "" {
		for _, node := range report.Nodes {
			if node.NeedsCert && node.CertMode == "none" {
				return fmt.Errorf("node %d requires a certificate; configure cert_config in the panel or a local certificate policy", node.ID)
			}
		}
	}
	return writeDeploymentReport(os.Stdout, report, *output)
}

// Environment changes are scoped to this short-lived inspection process and
// restored for callers and tests. The credentials themselves are never output.
func deploymentCredentials(path string) (func(), error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return func() {}, nil
	}
	if err != nil {
		return nil, errors.New("could not read installed credentials")
	}
	type previous struct {
		value string
		set   bool
	}
	old := map[string]previous{}
	restore := func() {
		for key, previous := range old {
			if previous.set {
				_ = os.Setenv(key, previous.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(key, "INSTANCE_") {
			continue
		}
		if _, saved := old[key]; !saved {
			before, exists := os.LookupEnv(key)
			old[key] = previous{before, exists}
		}
		if err := os.Setenv(key, value); err != nil {
			restore()
			return nil, errors.New("invalid installed credentials")
		}
	}
	return restore, nil
}

func inspectDeployment(configurations []config.Config, topology string) (*deploymentReport, error) {
	report := &deploymentReport{Nodes: []deploymentNode{}}
	for _, cfg := range configurations {
		if cfg.Standalone != nil && cfg.Standalone.Enabled {
			node, err := model.NodeSpecFromStandaloneValidated(&cfg)
			if err != nil {
				return nil, errors.New("invalid standalone node configuration")
			}
			view, err := inspectDeploymentNode(node, 0, cfg.Cert, topology)
			if err != nil {
				return nil, err
			}
			report.Nodes = append(report.Nodes, view)
			continue
		}
		pcfg := cfg.Panel
		ids := []panel.MachineNode{{ID: pcfg.NodeID, Type: pcfg.NodeType}}
		if cfg.Machine != nil {
			pcfg.Token, pcfg.MachineID = cfg.Machine.Token, cfg.Machine.MachineID
			list, err := panel.NewClient(pcfg).GetMachineNodes()
			if err != nil {
				return nil, fmt.Errorf("could not read machine nodes; check panel URL, machine ID and token: %w", err)
			}
			ids = list.Nodes
			if len(ids) == 0 {
				report.Warnings = append(report.Warnings, "This machine has no assigned nodes; listeners will start after nodes and users are assigned.")
			}
		}
		for _, entry := range ids {
			pcfg.NodeID, pcfg.NodeType = entry.ID, entry.Type
			client := panel.NewClient(pcfg)
			remote, err := client.GetConfig()
			if err != nil {
				return nil, fmt.Errorf("could not read node %d; check panel URL, node type, ID and token: %w", entry.ID, err)
			}
			if remote == nil {
				return nil, fmt.Errorf("node %d returned no configuration", entry.ID)
			}
			kcfg := cfg.Kernel
			if cfg.Machine != nil {
				kcfg.Type = model.ResolveKernelForTransport(remote.Network, kcfg.Type)
			}
			node, err := model.NodeSpecFromPanelValidated(remote, kcfg)
			if err != nil {
				return nil, fmt.Errorf("node %d configuration rejected: %w", entry.ID, err)
			}
			view, err := inspectDeploymentNode(node, entry.ID, cfg.Cert, topology)
			if err != nil {
				return nil, fmt.Errorf("node %d: %w", entry.ID, err)
			}
			users, err := client.GetUsers()
			if err != nil {
				return nil, fmt.Errorf("could not read users for node %d; check panel permissions: %w", entry.ID, err)
			}
			view.Users = len(users)
			if len(users) == 0 {
				report.Warnings = append(report.Warnings, fmt.Sprintf("Node %d has no users; the backend will not open its listener yet.", entry.ID))
			}
			report.Nodes = append(report.Nodes, view)
		}
	}
	for i, node := range report.Nodes {
		for _, previous := range report.Nodes[:i] {
			if deploymentListenersOverlap(node, previous) {
				return nil, fmt.Errorf("nodes %d and %d use the same listener; assign different panel ports", previous.ID, node.ID)
			}
		}
	}
	return report, nil
}

func deploymentListenersOverlap(a, b deploymentNode) bool {
	udp := func(protocol string) bool {
		return protocol == "hysteria" || protocol == "hysteria2" || protocol == "tuic"
	}
	if a.Port != b.Port || udp(a.Protocol) != udp(b.Protocol) {
		return false
	}
	if a.Listen == b.Listen || a.Listen == "::" || b.Listen == "::" {
		return true
	}
	ipA, ipB := net.ParseIP(a.Listen), net.ParseIP(b.Listen)
	if ipA == nil || ipB == nil {
		return false
	}
	return ipA.Equal(ipB) || ipA.To4() != nil && ipB.To4() != nil && (ipA.IsUnspecified() || ipB.IsUnspecified())
}

func inspectDeploymentNode(node *model.NodeSpec, id int, local config.CertConfig, topology string) (deploymentNode, error) {
	network := strings.ToLower(strings.TrimSpace(node.Network))
	if network == "" {
		network = "tcp"
	}
	view := deploymentNode{ID: id, Protocol: node.Protocol, Network: network, TLS: node.TLS, Port: node.ServerPort, Listen: node.ListenIP, Domain: node.ServerName, Users: -1}
	if view.Port < 1 || view.Port > 65535 {
		return view, errors.New("panel listener port must be between 1 and 65535")
	}
	if view.Listen == "" {
		view.Listen = "::"
	}
	if net.ParseIP(view.Listen) == nil {
		return view, errors.New("listener address must be a valid IP address")
	}
	if view.Domain == "" {
		view.Domain, _ = node.TLSSettings["server_name"].(string)
	}
	if view.Domain == "" {
		view.Domain = node.Host
	}
	if value, ok := node.NetworkSettings["host"].(string); ok && value != "" && view.Domain == "" {
		view.Domain = value
	}
	if value, ok := node.NetworkSettings["path"].(string); ok {
		view.Path = value
	}
	if (network == "xhttp" || network == "splithttp") && view.Path == "" {
		view.Path = "/"
	}
	certificate := local
	if node.CertConfig != nil {
		certificate = *node.CertConfig
		view.PanelCert = true
	}
	view.CertMode = effectiveDeploymentCertMode(certificate)
	if view.Domain == "" {
		view.Domain = certificate.Domain
	}
	if certificate.Domain == "" {
		certificate.Domain = view.Domain
	}
	view.NeedsCert = node.TLS == 1 || node.Protocol == "tuic" || node.Protocol == "hysteria" || node.Protocol == "hysteria2" || node.Protocol == "anytls" || node.Protocol == "trojan" && node.TLS != 2
	if view.NeedsCert {
		if view.PanelCert && view.CertMode == "none" {
			return view, errors.New("panel certificate mode is none while TLS is enabled; configure the panel certificate policy first")
		}
		if err := validateDeploymentCert(certificate); err != nil {
			return view, fmt.Errorf("certificate policy: %w", err)
		}
	}
	if node.TLS == 2 {
		private, _ := node.TLSSettings["private_key"].(string)
		serverName, _ := node.TLSSettings["server_name"].(string)
		destination, _ := node.TLSSettings["dest"].(string)
		if private == "" || serverName == "" && destination == "" {
			return view, errors.New("REALITY requires private_key and server_name or dest in the panel")
		}
	}
	if topology == "cloudflare" || topology == "cdn" || topology == "proxy" {
		if network != "xhttp" && network != "splithttp" && network != "ws" && network != "websocket" && network != "grpc" && network != "httpupgrade" {
			return view, errors.New("an HTTP CDN/reverse proxy requires an HTTP transport such as XHTTP, WebSocket or gRPC")
		}
		if node.TLS == 2 {
			return view, errors.New("a TLS-terminating HTTP CDN/reverse proxy cannot pass through REALITY; use TLS or a separate direct path")
		}
		if topology != "proxy" && node.TLS != 1 {
			return view, errors.New("HTTPS origin mode requires TLS enabled on the panel node; use proxy topology when Nginx/Caddy terminates TLS")
		}
		if topology == "cloudflare" {
			if view.CertMode == "self" {
				return view, errors.New("Cloudflare Full (strict) requires a public CA or Origin CA certificate; choose a different certificate policy")
			}
			allowed := map[int]bool{443: true, 2053: true, 2083: true, 2087: true, 2096: true, 8443: true}
			if !allowed[view.Port] {
				return view, errors.New("Cloudflare HTTPS proxy normally supports ports 443, 2053, 2083, 2087, 2096 and 8443; configure an origin rule or use one of these ports")
			}
		}
	}
	return view, nil
}

func writeDeploymentReport(out io.Writer, report *deploymentReport, format string) error {
	clean := func(value string) string {
		return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ", "\x1b", "").Replace(value)
	}
	field := func(value string) string {
		if value == "" {
			return "-"
		}
		return clean(value)
	}
	switch format {
	case "json":
		return json.NewEncoder(out).Encode(report)
	case "tsv":
		fmt.Fprintf(out, "NODES\t%d\n", len(report.Nodes))
		for _, node := range report.Nodes {
			fmt.Fprintf(out, "NODE\t%d\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%t\t%t\t%d\n", node.ID, field(node.Protocol), field(node.Network), node.TLS, node.Port, field(node.Listen), field(node.Domain), field(node.Path), field(node.CertMode), node.PanelCert, node.NeedsCert, node.Users)
		}
		for _, warning := range report.Warnings {
			fmt.Fprintf(out, "WARNING\t%s\n", clean(warning))
		}
	case "text":
		for _, node := range report.Nodes {
			fmt.Fprintf(out, "Node %d: %s/%s TLS=%d listen=%s:%d users=%d certificate=%s panel_certificate=%t\n", node.ID, clean(node.Protocol), clean(node.Network), node.TLS, clean(node.Listen), node.Port, node.Users, node.CertMode, node.PanelCert)
			fmt.Fprintf(out, "  Host/SNI=%s path=%s\n", field(node.Domain), field(node.Path))
		}
		for _, warning := range report.Warnings {
			fmt.Fprintf(out, "Notice: %s\n", clean(warning))
		}
	default:
		return errors.New("output must be text, json or tsv")
	}
	return nil
}

func effectiveDeploymentCertMode(cfg config.CertConfig) string {
	if cfg.CertMode != "" {
		return strings.ToLower(strings.TrimSpace(cfg.CertMode))
	}
	if cfg.AutoTLS {
		return "http"
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		return "file"
	}
	return "none"
}

func validateDeploymentCert(cfg config.CertConfig) error {
	mode := effectiveDeploymentCertMode(cfg)
	if mode == "none" {
		return nil
	}
	if cfg.Domain != "" {
		if strings.ContainsAny(cfg.Domain, " /:@\r\n\t") || net.ParseIP(cfg.Domain) != nil || !strings.Contains(cfg.Domain, ".") {
			return errors.New("certificate domain must be a DNS hostname")
		}
		domain := strings.TrimSuffix(cfg.Domain, ".")
		if mode == "dns" {
			domain = strings.TrimPrefix(domain, "*.")
		}
		if len(domain) > 253 {
			return errors.New("certificate domain is too long")
		}
		for _, label := range strings.Split(domain, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return errors.New("invalid certificate hostname; use ASCII or punycode labels")
			}
			for _, character := range label {
				if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-') {
					return errors.New("invalid certificate hostname; use ASCII or punycode labels")
				}
			}
		}
	}
	switch mode {
	case "file", "content":
		certificate, key := []byte(cfg.CertContent), []byte(cfg.KeyContent)
		if mode == "file" {
			var err error
			certificate, err = os.ReadFile(cfg.CertFile)
			if err != nil {
				return errors.New("could not read certificate file")
			}
			key, err = os.ReadFile(cfg.KeyFile)
			if err != nil {
				return errors.New("could not read private key file")
			}
		}
		pair, err := tls.X509KeyPair(certificate, key)
		if err != nil {
			return errors.New("certificate and private key are invalid or do not match")
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return errors.New("invalid certificate")
		}
		now := time.Now()
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return errors.New("certificate is expired or not yet valid; check server time")
		}
		if cfg.Domain != "" && leaf.VerifyHostname(cfg.Domain) != nil {
			return errors.New("certificate does not cover the selected domain")
		}
	case "http", "dns", "self":
		if cfg.Domain == "" {
			return errors.New("a certificate domain is required")
		}
		if mode == "http" && (cfg.HTTPPort < 0 || cfg.HTTPPort > 65535) {
			return errors.New("invalid HTTP challenge port")
		}
		if mode == "dns" && cfg.DNSProvider == "" {
			return errors.New("DNS certificate mode requires a provider")
		}
		if mode == "dns" {
			provider, ok := dnsproviders.Get(cfg.DNSProvider)
			if !ok {
				return errors.New("DNS certificate provider is not supported by this build")
			}
			if _, err := provider.Build(cfg.DNSEnv); err != nil {
				return errors.New("DNS certificate provider credentials are missing or invalid")
			}
		}
	default:
		return errors.New("unsupported certificate mode")
	}
	return nil
}

func readDeploymentCert(path string) (*config.CertConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("could not read certificate profile")
	}
	var profile config.CertConfig
	if yaml.Unmarshal(data, &profile) != nil {
		return nil, errors.New("invalid certificate profile")
	}
	if err := validateDeploymentCert(profile); err != nil {
		return nil, err
	}
	return &profile, nil
}

func runDeploymentCert(args []string) error {
	fs := flag.NewFlagSet("deploy cert", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("mode", "file", "file, dns, http, self or none")
	domain := fs.String("domain", "", "certificate hostname")
	certFile := fs.String("cert-file", "", "existing certificate")
	keyFile := fs.String("key-file", "", "existing private key")
	email := fs.String("email", "", "ACME email")
	secretFile := fs.String("dns-token-file", "", "Cloudflare DNS token file")
	httpPort := fs.Int("http-port", 80, "HTTP challenge port")
	output := fs.String("output", "", "private profile output")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *output == "" {
		return errors.New("invalid deploy cert arguments; provide --output")
	}
	profile := config.CertConfig{CertMode: *mode, Domain: *domain, CertFile: *certFile, KeyFile: *keyFile, Email: *email, HTTPPort: *httpPort}
	if *mode == "dns" {
		secret, err := readDeploymentSecret(*secretFile)
		if err != nil {
			return err
		}
		profile.DNSProvider = "cloudflare"
		profile.DNSEnv = map[string]string{"CLOUDFLARE_DNS_API_TOKEN": secret}
	}
	if err := validateDeploymentCert(profile); err != nil {
		return err
	}
	data, err := yaml.Marshal(profile)
	if err != nil {
		return errors.New("could not encode certificate profile")
	}
	if err := os.WriteFile(*output, data, 0600); err != nil {
		return errors.New("could not write certificate profile")
	}
	return os.Chmod(*output, 0600)
}
