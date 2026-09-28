// Package hostinfo reads basic host figures from /proc and statfs.
package hostinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Info is a point-in-time host summary.
type Info struct {
	Hostname     string
	Kernel       string
	Uptime       time.Duration
	Load1        float64
	CPUs         int
	MemTotal     int64
	MemAvailable int64
	SwapTotal    int64
	SwapFree     int64
	DiskTotal    int64
	DiskFree     int64
}

// MemUsedPct is the used memory percentage.
func (i Info) MemUsedPct() float64 { return pct(i.MemTotal-i.MemAvailable, i.MemTotal) }

// DiskUsedPct is the used disk percentage of the data root.
func (i Info) DiskUsedPct() float64 { return pct(i.DiskTotal-i.DiskFree, i.DiskTotal) }

func pct(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

// Read collects host figures; missing values stay zero.
func Read(dataRoot string) Info {
	var i Info
	i.Hostname, _ = os.Hostname()
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		i.Kernel = unix.ByteSliceToString(u.Release[:])
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			s, _ := strconv.ParseFloat(f[0], 64)
			i.Uptime = time.Duration(s) * time.Second
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			i.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	var set unix.CPUSet
	if unix.SchedGetaffinity(0, &set) == nil {
		i.CPUs = set.Count()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), ":")
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			switch k {
			case "MemTotal":
				i.MemTotal = n << 10
			case "MemAvailable":
				i.MemAvailable = n << 10
			case "SwapTotal":
				i.SwapTotal = n << 10
			case "SwapFree":
				i.SwapFree = n << 10
			}
		}
		f.Close()
	}
	var st unix.Statfs_t
	if unix.Statfs(dataRoot, &st) == nil {
		i.DiskTotal = int64(st.Blocks) * st.Bsize
		i.DiskFree = int64(st.Bavail) * st.Bsize
	}
	return i
}
