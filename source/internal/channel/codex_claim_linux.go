package channel

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func codexClaimProcessStart(pid int) (time.Time, error) {
	data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return time.Time{}, e
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return time.Time{}, errors.New("invalid process")
	}
	fields := strings.Fields(string(data)[end+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return time.Time{}, os.ErrNotExist
	}
	ticks, e := strconv.ParseInt(fields[19], 10, 64)
	if e != nil {
		return time.Time{}, e
	}
	boot, e := os.ReadFile("/proc/stat")
	if e != nil {
		return time.Time{}, e
	}
	for _, line := range strings.Split(string(boot), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		seconds, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil {
			return time.Time{}, e
		}
		// Linux exports /proc starttime in USER_HZ (100 on supported Linux).
		return time.Unix(seconds+ticks/100, ticks%100*int64(10*time.Millisecond)), nil
	}
	return time.Time{}, errors.New("boot time unavailable")
}
