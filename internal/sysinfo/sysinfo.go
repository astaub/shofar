// Package sysinfo reads macOS system memory state: totals, the reclaimable
// "available" figure, swap usage, and the kernel's VM pressure level. It shells
// out to vm_stat and sysctl rather than linking against the Mach APIs so the
// tool stays a single dependency-free binary that is easy to audit.
package sysinfo

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Pressure mirrors the kern.memorystatus_vm_pressure_level sysctl, whose values
// are the kernel's VM_PRESSURE levels.
type Pressure int

const (
	PressureUnknown  Pressure = 0
	PressureNormal   Pressure = 1
	PressureWarning  Pressure = 2
	PressureCritical Pressure = 4
)

func (p Pressure) String() string {
	switch p {
	case PressureNormal:
		return "normal"
	case PressureWarning:
		return "warning"
	case PressureCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Memory is a point-in-time snapshot of physical memory accounting, in bytes.
//
// Available is the figure the capacity model reasons about: physical memory the
// system can hand out without paging out live working sets. On macOS we treat
// wired + active + compressed as genuinely in-use and everything else
// (free + inactive + speculative + purgeable) as reclaimable, which matches the
// "used vs available" split Activity Monitor presents.
type Memory struct {
	TotalBytes      uint64   `json:"total_bytes"`
	AvailableBytes  uint64   `json:"available_bytes"`
	UsedBytes       uint64   `json:"used_bytes"`
	FreeBytes       uint64   `json:"free_bytes"`
	WiredBytes      uint64   `json:"wired_bytes"`
	ActiveBytes     uint64   `json:"active_bytes"`
	InactiveBytes   uint64   `json:"inactive_bytes"`
	CompressedBytes uint64   `json:"compressed_bytes"`
	SwapUsedBytes   uint64   `json:"swap_used_bytes"`
	Pressure        Pressure `json:"-"`
	PressureName    string   `json:"pressure"`
}

// Read collects a fresh memory snapshot.
func Read() (Memory, error) {
	var m Memory

	total, err := sysctlUint("hw.memsize")
	if err != nil {
		return m, fmt.Errorf("read hw.memsize: %w", err)
	}
	m.TotalBytes = total

	pageSize, counts, err := readVMStat()
	if err != nil {
		return m, err
	}
	m.FreeBytes = counts["free"] * pageSize
	m.ActiveBytes = counts["active"] * pageSize
	m.InactiveBytes = counts["inactive"] * pageSize
	m.WiredBytes = counts["wired down"] * pageSize
	m.CompressedBytes = counts["occupied by compressor"] * pageSize

	m.UsedBytes = m.WiredBytes + m.ActiveBytes + m.CompressedBytes
	if m.UsedBytes > m.TotalBytes {
		m.UsedBytes = m.TotalBytes
	}
	m.AvailableBytes = m.TotalBytes - m.UsedBytes

	// Pressure is best-effort for the snapshot, but it must NOT silently
	// default to "normal" on read failure: the capacity gate treats unknown
	// pressure as a blocking signal (fail closed) rather than approving new
	// work when the kernel state is uncertain.
	if lvl, err := sysctlUint("kern.memorystatus_vm_pressure_level"); err == nil {
		m.Pressure = Pressure(lvl)
	} else {
		m.Pressure = PressureUnknown
	}
	m.PressureName = m.Pressure.String()

	m.SwapUsedBytes = readSwapUsed()

	return m, nil
}

// Absolute paths to the macOS system tools we shell out to. Using absolute
// paths prevents a hijacked PATH from feeding this process-killing tool forged
// memory or process data.
const (
	binSysctl = "/usr/sbin/sysctl"
	binVMStat = "/usr/bin/vm_stat"
)

func sysctlUint(key string) (uint64, error) {
	out, err := exec.Command(binSysctl, "-n", key).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

// readVMStat parses `vm_stat` into a page size and a map of page counts keyed by
// the suffix of each "Pages <suffix>:" line (e.g. "free", "wired down",
// "occupied by compressor").
func readVMStat() (pageSize uint64, counts map[string]uint64, err error) {
	out, err := exec.Command(binVMStat).Output()
	if err != nil {
		return 0, nil, fmt.Errorf("run vm_stat: %w", err)
	}
	pageSize, counts = parseVMStat(string(out))
	if pageSize == 0 {
		return 0, nil, fmt.Errorf("vm_stat: could not determine page size")
	}
	return pageSize, counts, nil
}

// parseVMStat is split out from readVMStat so it can be unit tested against
// captured fixtures.
func parseVMStat(out string) (pageSize uint64, counts map[string]uint64) {
	counts = make(map[string]uint64)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			// "... (page size of 16384 bytes)"
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				fields := strings.Fields(rest)
				if len(fields) > 0 {
					pageSize, _ = strconv.ParseUint(fields[0], 10, 64)
				}
			}
			continue
		}
		key, valStr, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !strings.HasPrefix(key, "Pages ") {
			continue
		}
		suffix := strings.TrimPrefix(key, "Pages ")
		valStr = strings.TrimSpace(valStr)
		valStr = strings.TrimSuffix(valStr, ".")
		if n, err := strconv.ParseUint(valStr, 10, 64); err == nil {
			counts[suffix] = n
		}
	}
	return pageSize, counts
}

// readSwapUsed parses the "used = N" field of vm.swapusage. Returns 0 on any
// parse failure since swap is advisory in the capacity model.
func readSwapUsed() uint64 {
	out, err := exec.Command(binSysctl, "-n", "vm.swapusage").Output()
	if err != nil {
		return 0
	}
	return parseSwapUsed(string(out))
}

func parseSwapUsed(s string) uint64 {
	// Example: "total = 2048.00M  used = 512.00M  free = 1536.00M  (encrypted)"
	fields := strings.Fields(s)
	for i := 0; i < len(fields)-2; i++ {
		if fields[i] == "used" && fields[i+1] == "=" {
			return parseHumanBytes(fields[i+2])
		}
	}
	return 0
}

// parseHumanBytes converts a sysctl swap figure like "512.00M" or "1.50G" into
// bytes.
func parseHumanBytes(s string) uint64 {
	if s == "" {
		return 0
	}
	unit := s[len(s)-1]
	mult := float64(1)
	switch unit {
	case 'K', 'k':
		mult = 1 << 10
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1 << 30
		s = s[:len(s)-1]
	case 'T', 't':
		mult = 1 << 40
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return uint64(v * mult)
}
