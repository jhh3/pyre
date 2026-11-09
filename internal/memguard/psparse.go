package memguard

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// parsePS parses `ps -o pid=,ppid=,rss=,lstart=,comm=`; lstart is five
// fields ("Sat Oct 24 10:01:02 2026"), comm the rest of the line.
func parsePS(out []byte) map[int]Proc {
	m := map[int]Proc{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 9 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		var start uint64
		if t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[3:8], " "), time.Local); err == nil {
			start = uint64(t.Unix())
		}
		name := strings.Join(f[8:], " ")
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		m[pid] = Proc{PID: pid, PPID: ppid, RSS: rss << 10, Start: start, Name: name}
	}
	return m
}
