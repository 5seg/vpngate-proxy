package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	vpngateAPIURL   = "http://www.vpngate.net/api/iphone/"
	connectTimeout  = 15 * time.Second
	cooldownPeriod  = 24 * time.Hour
	proxyPort       = 1080
	apiPort         = 8080
	historyFilePath = "/data/history.json"
	ovpnTempPath    = "/tmp/current.ovpn"
)

type ServerInfo struct {
	HostName     string    `json:"hostname"`
	IP           string    `json:"ip"`
	Score        int64     `json:"score"`
	Ping         int64     `json:"ping"`
	Speed        int64     `json:"speed"`
	CountryLong  string    `json:"country_long"`
	CountryShort string    `json:"country_short"`
	ConfigData   string    `json:"-"`
	ConnectedAt  time.Time `json:"connected_at,omitempty"`
}

type PoolStatus struct {
	TotalJPServers     int `json:"total_jp_servers"`
	BlacklistedLast24h int `json:"blacklisted_last_24h"`
	AvailableCandidates int `json:"available_candidates"`
}

type StatusResponse struct {
	Status        string      `json:"status"`
	CurrentServer *ServerInfo `json:"current_server"`
	PublicIP      string      `json:"public_ip,omitempty"`
	Pool          PoolStatus  `json:"pool"`
	LastError     string      `json:"last_error,omitempty"`
}

type Manager struct {
	mu            sync.RWMutex
	status        string // "disconnected", "connecting", "connected", "rotating", "exhausted", "error"
	currentServer *ServerInfo
	publicIP      string
	lastError     string
	history       map[string]time.Time // IP -> DisconnectedAt
	historyFile   string
	ovpnCmd       *exec.Cmd
	allJPServers  []ServerInfo
	lastFetchTime time.Time
}

func NewManager(historyFile string) *Manager {
	m := &Manager{
		status:      "disconnected",
		history:     make(map[string]time.Time),
		historyFile: historyFile,
	}
	m.loadHistory()
	return m
}

func (m *Manager) loadHistory() {
	if m.historyFile == "" {
		return
	}
	data, err := os.ReadFile(m.historyFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[History] Error reading %s: %v", m.historyFile, err)
		}
		return
	}
	var loaded map[string]time.Time
	if err := json.Unmarshal(data, &loaded); err == nil {
		now := time.Now()
		for ip, t := range loaded {
			if now.Sub(t) < cooldownPeriod {
				m.history[ip] = t
			}
		}
		log.Printf("[History] Loaded %d active cooldown entries", len(m.history))
	}
}

func (m *Manager) saveHistory() {
	if m.historyFile == "" {
		return
	}
	data, err := json.MarshalIndent(m.history, "", "  ")
	if err != nil {
		log.Printf("[History] Error marshaling history: %v", err)
		return
	}
	dir := filepath.Dir(m.historyFile)
	_ = os.MkdirAll(dir, 0755)
	if err := os.WriteFile(m.historyFile, data, 0644); err != nil {
		log.Printf("[History] Error writing history: %v", err)
	}
}

func (m *Manager) purgeHistory() {
	now := time.Now()
	changed := false
	for ip, t := range m.history {
		if now.Sub(t) >= cooldownPeriod {
			delete(m.history, ip)
			changed = true
		}
	}
	if changed {
		m.saveHistory()
	}
}

func (m *Manager) fetchServers() ([]ServerInfo, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(vpngateAPIURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch VPNGate CSV: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from VPNGate", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	lines := strings.Split(string(body), "\n")
	var csvLines []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "*") || l == "" {
			continue
		}
		if strings.HasPrefix(l, "#") {
			l = strings.TrimPrefix(l, "#")
		}
		csvLines = append(csvLines, l)
	}

	reader := csv.NewReader(strings.NewReader(strings.Join(csvLines, "\n")))
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing CSV: %w", err)
	}

	if len(records) < 2 {
		return nil, errors.New("CSV contains no data rows")
	}

	header := records[0]
	idxMap := make(map[string]int)
	for i, h := range header {
		idxMap[strings.TrimSpace(h)] = i
	}

	reqCols := []string{"HostName", "IP", "CountryShort", "OpenVPN_ConfigData_Base64"}
	for _, rc := range reqCols {
		if _, ok := idxMap[rc]; !ok {
			return nil, fmt.Errorf("missing required column: %s", rc)
		}
	}

	var servers []ServerInfo
	for _, row := range records[1:] {
		if len(row) <= idxMap["CountryShort"] {
			continue
		}
		country := strings.ToUpper(strings.TrimSpace(row[idxMap["CountryShort"]]))
		if country != "JP" {
			continue
		}

		b64Config := strings.TrimSpace(row[idxMap["OpenVPN_ConfigData_Base64"]])
		if b64Config == "" {
			continue
		}

		cfgBytes, err := base64.StdEncoding.DecodeString(b64Config)
		if err != nil {
			continue
		}

		s := ServerInfo{
			HostName:     row[idxMap["HostName"]],
			IP:           row[idxMap["IP"]],
			CountryShort: country,
			ConfigData:   string(cfgBytes),
		}

		if idx, ok := idxMap["CountryLong"]; ok && len(row) > idx {
			s.CountryLong = row[idx]
		}
		if idx, ok := idxMap["Score"]; ok && len(row) > idx {
			s.Score, _ = strconv.ParseInt(row[idx], 10, 64)
		}
		if idx, ok := idxMap["Ping"]; ok && len(row) > idx {
			s.Ping, _ = strconv.ParseInt(row[idx], 10, 64)
		}
		if idx, ok := idxMap["Speed"]; ok && len(row) > idx {
			s.Speed, _ = strconv.ParseInt(row[idx], 10, 64)
		}

		servers = append(servers, s)
	}

	return servers, nil
}

func (m *Manager) updateServersPoolLocked() error {
	if time.Since(m.lastFetchTime) < 5*time.Minute && len(m.allJPServers) > 0 {
		return nil
	}
	servers, err := m.fetchServers()
	if err != nil {
		if len(m.allJPServers) > 0 {
			log.Printf("[VPNGate] Server fetch failed: %v. Using cached %d servers", err, len(m.allJPServers))
			return nil
		}
		return err
	}
	m.allJPServers = servers
	m.lastFetchTime = time.Now()
	log.Printf("[VPNGate] Fetched %d JP servers", len(m.allJPServers))
	return nil
}

func (m *Manager) GetStatus() StatusResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()

	blacklisted := 0
	now := time.Now()
	for _, t := range m.history {
		if now.Sub(t) < cooldownPeriod {
			blacklisted++
		}
	}

	avail := 0
	for _, s := range m.allJPServers {
		if t, ok := m.history[s.IP]; !ok || now.Sub(t) >= cooldownPeriod {
			avail++
		}
	}

	var cur *ServerInfo
	if m.currentServer != nil {
		cp := *m.currentServer
		cur = &cp
	}

	return StatusResponse{
		Status:        m.status,
		CurrentServer: cur,
		PublicIP:      m.publicIP,
		Pool: PoolStatus{
			TotalJPServers:      len(m.allJPServers),
			BlacklistedLast24h:  blacklisted,
			AvailableCandidates: avail,
		},
		LastError: m.lastError,
	}
}

func (m *Manager) stopCurrentVPNLocked() {
	if m.ovpnCmd != nil && m.ovpnCmd.Process != nil {
		log.Printf("[OpenVPN] Stopping active VPN process (PID: %d)...", m.ovpnCmd.Process.Pid)
		_ = m.ovpnCmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- m.ovpnCmd.Wait() }()
		select {
		case <-done:
			log.Printf("[OpenVPN] Process exited")
		case <-time.After(3 * time.Second):
			log.Printf("[OpenVPN] Process didn't exit, sending SIGKILL")
			_ = m.ovpnCmd.Process.Kill()
		}
		m.ovpnCmd = nil
	}

	if m.currentServer != nil {
		m.history[m.currentServer.IP] = time.Now()
		m.saveHistory()
		log.Printf("[History] Blacklisted %s for 24h", m.currentServer.IP)
		m.currentServer = nil
	}
	m.publicIP = ""
}

func (m *Manager) Rotate() error {
	m.mu.Lock()
	if m.status == "connecting" || m.status == "rotating" {
		m.mu.Unlock()
		return errors.New("CONFLICT")
	}

	if m.status == "connected" {
		m.status = "rotating"
	} else {
		m.status = "connecting"
	}
	m.stopCurrentVPNLocked()
	m.mu.Unlock()

	// Try connecting in a loop with fallback
	for {
		m.mu.Lock()
		m.purgeHistory()
		if err := m.updateServersPoolLocked(); err != nil {
			m.status = "error"
			m.lastError = err.Error()
			m.mu.Unlock()
			return fmt.Errorf("failed to get servers: %w", err)
		}

		var candidates []ServerInfo
		now := time.Now()
		for _, s := range m.allJPServers {
			if t, ok := m.history[s.IP]; !ok || now.Sub(t) >= cooldownPeriod {
				candidates = append(candidates, s)
			}
		}

		if len(candidates) == 0 {
			m.status = "exhausted"
			m.lastError = "all JP servers in cooldown or no JP servers found"
			m.mu.Unlock()
			log.Printf("[Rotate ERROR] No available JP servers left (all in 24h cooldown)")
			return errors.New("EXHAUSTED")
		}

		// Pick random candidate
		target := candidates[rand.Intn(len(candidates))]
		m.mu.Unlock()

		log.Printf("[Rotate] Attempting connection to %s (%s)...", target.HostName, target.IP)
		err := m.connectVPN(target)
		if err == nil {
			log.Printf("[Rotate] Successfully connected to %s (%s)", target.HostName, target.IP)
			return nil
		}

		log.Printf("[Rotate] Failed to connect to %s (%s): %v. Blacklisting and trying next...", target.HostName, target.IP, err)
		m.mu.Lock()
		m.history[target.IP] = time.Now()
		m.saveHistory()
		m.stopCurrentVPNLocked()
		m.mu.Unlock()
	}
}

func (m *Manager) connectVPN(target ServerInfo) error {
	cleanConfig := sanitizeOVPNConfig(target.ConfigData)
	if err := os.WriteFile(ovpnTempPath, []byte(cleanConfig), 0600); err != nil {
		return fmt.Errorf("writing ovpn file: %w", err)
	}

	cmd := exec.Command("openvpn",
		"--config", ovpnTempPath,
		"--script-security", "2",
		"--verb", "3",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe error: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting openvpn: %w", err)
	}

	m.mu.Lock()
	m.ovpnCmd = cmd
	m.mu.Unlock()

	connectedChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("[OpenVPN] %s", line)
			if strings.Contains(line, "Initialization Sequence Completed") {
				select {
				case connectedChan <- true:
				default:
				}
			}
		}
	}()

	select {
	case <-connectedChan:
		// Connection established, verify external connectivity
		pubIP, err := fetchPublicIP()
		if err != nil {
			log.Printf("[Verify] Connected but public IP lookup failed: %v", err)
			return fmt.Errorf("public IP check failed: %w", err)
		}

		m.mu.Lock()
		m.status = "connected"
		target.ConnectedAt = time.Now()
		m.currentServer = &target
		m.publicIP = pubIP
		m.lastError = ""
		m.mu.Unlock()
		log.Printf("[Verify] Verified external public IP: %s", pubIP)
		return nil

	case <-time.After(connectTimeout):
		return errors.New("connection timeout (15s)")

	case err := <-errChan:
		return fmt.Errorf("openvpn process failed: %w", err)
	}
}

func sanitizeOVPNConfig(raw string) string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		// Strip harmful or conflicting options if present
		if strings.HasPrefix(trimmed, "auth-user-pass") && len(strings.Fields(trimmed)) > 1 {
			// Some configs have auth-user-pass with filename, replace with empty
			lines = append(lines, "auth-user-pass")
			continue
		}
		lines = append(lines, line)
	}
	// VPNGate uses dummy credentials 'vpn'/'vpn'
	lines = append(lines, "auth-user-pass /tmp/vpnpass.txt")
	return strings.Join(lines, "\n")
}

func fetchPublicIP() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	endpoints := []string{
		"http://api.ipify.org",
		"http://ifconfig.me/ip",
		"http://checkip.amazonaws.com",
	}

	for _, ep := range endpoints {
		resp, err := client.Get(ep)
		if err == nil && resp.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			ip := strings.TrimSpace(string(body))
			if net.ParseIP(ip) != nil {
				return ip, nil
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return "", errors.New("all public IP lookup endpoints failed")
}

// HTTP / HTTPS CONNECT Proxy
func startProxyServer(m *Manager, port int) *http.Server {
	server := &http.Server{
		Addr: fmt.Sprintf(":%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.mu.RLock()
			status := m.status
			m.mu.RUnlock()

			if status == "connecting" || status == "rotating" {
				w.Header().Set("X-Proxy-Status", "connecting")
				http.Error(w, "Proxy is connecting to VPN, please retry shortly", http.StatusServiceUnavailable)
				return
			}
			if status != "connected" {
				w.Header().Set("X-Proxy-Status", status)
				http.Error(w, fmt.Sprintf("Proxy not ready (status: %s)", status), http.StatusServiceUnavailable)
				return
			}

			if r.Method == http.MethodConnect {
				handleHTTPSConnect(w, r)
			} else {
				handleHTTP(w, r)
			}
		}),
	}

	go func() {
		log.Printf("[Proxy] HTTP/HTTPS Proxy listening on :%d", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[Proxy] Listen error: %v", err)
		}
	}()

	return server
}

func handleHTTPSConnect(w http.ResponseWriter, r *http.Request) {
	destConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to connect to destination: %v", err), http.StatusServiceUnavailable)
		return
	}
	defer destConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("Hijacking failed: %v", err), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(destConn, clientConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, destConn)
	}()
	wg.Wait()
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	outReq := r.Clone(r.Context())
	if outReq.URL.Scheme == "" {
		outReq.URL.Scheme = "http"
	}
	if outReq.URL.Host == "" {
		outReq.URL.Host = r.Host
	}

	// Hop-by-hop headers
	outReq.RequestURI = ""
	outReq.Header.Del("Proxy-Connection")

	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("RoundTrip failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// REST API Server
func startAPIServer(m *Manager, port int) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		status := m.GetStatus()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(status)
	})

	mux.HandleFunc("/rotate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		err := m.Rotate()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			if err.Error() == "CONFLICT" {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "already rotating or connecting"})
				return
			}
			if err.Error() == "EXHAUSTED" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "no available servers left (24h cooldown exhausted)"})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		log.Printf("[API] REST API listening on :%d", port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[API] Listen error: %v", err)
		}
	}()

	return server
}

func main() {
	rand.Seed(time.Now().UnixNano())

	// Write dummy auth file for OpenVPN (VPNGate default credentials)
	_ = os.WriteFile("/tmp/vpnpass.txt", []byte("vpn\nvpn\n"), 0600)

	manager := NewManager(historyFilePath)

	// Start servers
	proxySrv := startProxyServer(manager, proxyPort)
	apiSrv := startAPIServer(manager, apiPort)

	// Initial connect in background
	go func() {
		log.Println("[Init] Initiating initial VPN connection to JP server...")
		if err := manager.Rotate(); err != nil {
			log.Printf("[Init ERROR] Initial connection failed: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[Shutdown] Signal received, stopping servers...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = proxySrv.Shutdown(ctx)
	_ = apiSrv.Shutdown(ctx)

	manager.mu.Lock()
	manager.stopCurrentVPNLocked()
	manager.mu.Unlock()

	log.Println("[Shutdown] Done")
}
