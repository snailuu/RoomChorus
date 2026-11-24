package netutil

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

// LocalHostname returns the macOS Bonjour LocalHostName (without .local suffix).
func LocalHostname() string {
	out, err := exec.Command("scutil", "--get", "LocalHostName").Output()
	if err == nil {
		name := sanitizeHostLabel(string(out))
		if name != "" {
			return name
		}
	}
	host, err := os.Hostname()
	if err == nil {
		host = strings.TrimSuffix(host, ".local")
		name := sanitizeHostLabel(host)
		if name != "" {
			return name
		}
	}
	return "roomchorus"
}

func sanitizeHostLabel(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		case r == '_' || r == ' ':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// MDNSAdvertiser registers RoomChorus on macOS Bonjour via /usr/bin/dns-sd while running.
type MDNSAdvertiser struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

// StartMDNSAdvertiser publishes `_http._tcp` on the given port via macOS dns-sd.
func StartMDNSAdvertiser(serviceName string, port int) *MDNSAdvertiser {
	adv := &MDNSAdvertiser{}
	if _, err := os.Stat("/usr/bin/dns-sd"); err != nil {
		return adv
	}
	if serviceName == "" {
		serviceName = "RoomChorus"
	}
	cmd := exec.Command("/usr/bin/dns-sd", "-R", serviceName, "_http._tcp", "local", intToStr(port))
	if err := cmd.Start(); err == nil {
		adv.cmd = cmd
	}
	return adv
}

// Stop terminates the background dns-sd registration process.
func (a *MDNSAdvertiser) Stop() {
	a.mu.Lock()
	cmd := a.cmd
	a.cmd = nil
	a.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func intToStr(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
