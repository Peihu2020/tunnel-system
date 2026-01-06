package utils

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// GetHostname returns the hostname of the system
func GetHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// GetLocalIP returns the first non-loopback IPv4 address
func GetLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}

	return "127.0.0.1"
}

// GetOSInfo returns operating system information
func GetOSInfo() string {
	return runtime.GOOS
}

// GetMachineID reads machine ID from /etc/machine-id
func GetMachineID() string {
	data, err := ioutil.ReadFile("/etc/machine-id")
	if err != nil {
		// Try fallback for different OS
		data, err = ioutil.ReadFile("/var/lib/dbus/machine-id")
		if err != nil {
			return "unknown"
		}
	}

	id := strings.TrimSpace(string(data))
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// GenerateAgentID generates a unique agent ID
func GenerateAgentID(prefix string) string {
	hostname := GetHostname()
	machineID := GetMachineID()
	timestamp := time.Now().Unix()

	if prefix == "" {
		prefix = "agent"
	}

	return fmt.Sprintf("%s-%s-%s-%d", prefix, hostname, machineID, timestamp)
}

// ValidatePort checks if port is valid
func ValidatePort(port int) bool {
	return port > 0 && port <= 65535
}

// ParsePorts parses port string like "3000,8080,9000-9010"
func ParsePorts(portStr string) ([]int, error) {
	var ports []int

	parts := strings.Split(portStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			// Port range
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid port range: %s", part)
			}

			start, err := parsePortNumber(rangeParts[0])
			if err != nil {
				return nil, err
			}

			end, err := parsePortNumber(rangeParts[1])
			if err != nil {
				return nil, err
			}

			if start > end {
				return nil, fmt.Errorf("invalid port range: start > end")
			}

			for port := start; port <= end; port++ {
				ports = append(ports, port)
			}
		} else {
			// Single port
			port, err := parsePortNumber(part)
			if err != nil {
				return nil, err
			}
			ports = append(ports, port)
		}
	}

	return ports, nil
}

func parsePortNumber(s string) (int, error) {
	var port int
	_, err := fmt.Sscanf(s, "%d", &port)
	if err != nil {
		return 0, fmt.Errorf("invalid port number: %s", s)
	}

	if !ValidatePort(port) {
		return 0, fmt.Errorf("port out of range: %d", port)
	}

	return port, nil
}

// CheckServiceAvailability checks if a local service is listening on a port
func CheckServiceAvailability(host string, port int, timeout time.Duration) bool {
	address := fmt.Sprintf("%s:%d", host, port)

	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}

	conn.Close()
	return true
}

// GetCPUUsage returns current CPU usage percentage
func GetCPUUsage() float64 {
	// Simplified CPU usage - in production use proper metrics
	// This is a placeholder implementation
	return 0.0
}

// GetMemoryUsage returns current memory usage percentage
func GetMemoryUsage() float64 {
	// Simplified memory usage - in production use proper metrics
	// This is a placeholder implementation
	return 0.0
}

// GetDiskUsage returns disk usage for a path
func GetDiskUsage(path string) (float64, error) {
	// Simplified disk usage - in production use proper metrics
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return 0, err
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used := total - free

	return (float64(used) / float64(total)) * 100, nil
}

// SanitizeServiceName ensures service name is valid
func SanitizeServiceName(name string) string {
	// Remove invalid characters
	invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|"}
	for _, char := range invalidChars {
		name = strings.ReplaceAll(name, char, "-")
	}

	// Trim and limit length
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		name = name[:64]
	}

	return name
}

// Retry executes a function with retry logic
func Retry(attempts int, delay time.Duration, fn func() error) error {
	var err error

	for i := 0; i < attempts; i++ {
		err = fn()
		if err == nil {
			return nil
		}

		if i < attempts-1 {
			time.Sleep(delay)
		}
	}

	return fmt.Errorf("after %d attempts, last error: %v", attempts, err)
}

// ByteCountSI converts bytes to human readable string (SI units)
func ByteCountSI(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}
