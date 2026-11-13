package piruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func processStart(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return "", ErrScope
	}
	fields := strings.Fields(string(data)[end+2:])
	if len(fields) <= 19 {
		return "", ErrScope
	}
	return fields[19], nil
}
func processNamespace(pid int, name string) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "ns", name))
}
func processCgroup(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", ErrScope
}
func verifyProcess(observation HostObservation) error {
	start, err := processStart(observation.MainPID)
	if err != nil || start != observation.PIDStart {
		return ErrScope
	}
	mount, err := processNamespace(observation.MainPID, "mnt")
	if err != nil || mount != observation.MountNamespace {
		return ErrScope
	}
	selfMount, err := processNamespace(os.Getpid(), "mnt")
	if err != nil || mount == selfMount {
		return ErrScope
	}
	user, err := processNamespace(observation.MainPID, "user")
	if err != nil || user != observation.UserNamespace {
		return ErrScope
	}
	group, err := processCgroup(observation.MainPID)
	if err != nil || group != observation.ControlGroup {
		return ErrScope
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) != observation.BootID {
		return ErrScope
	}
	selfUser, err := processNamespace(os.Getpid(), "user")
	if err != nil || user != selfUser {
		return ErrScope
	}
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(observation.MainPID), "status"))
	if err != nil {
		return err
	}
	var uid int
	var haveUID, haveGroups, haveAmbient bool
	var groups []int
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return ErrScope
			}
			uid, err = strconv.Atoi(fields[1])
			if err != nil {
				return ErrScope
			}
			haveUID = true
		}
		if strings.HasPrefix(line, "Groups:") {
			haveGroups = true
			for _, field := range strings.Fields(line)[1:] {
				id, err := strconv.Atoi(field)
				if err != nil {
					return ErrScope
				}
				groups = append(groups, id)
			}
		}
		if strings.HasPrefix(line, "CapAmb:") {
			haveAmbient = true
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[1] != "0000000000000000" {
				return ErrScope
			}
		}
	}
	want, err := os.Getgroups()
	if err != nil {
		return err
	}
	slices.Sort(groups)
	slices.Sort(want)
	if !haveUID || !haveGroups || !haveAmbient {
		return ErrScope
	}
	if current, err := processStart(observation.MainPID); err != nil || current != start {
		return ErrScope
	}
	if uid != os.Geteuid() || !slices.Equal(groups, want) {
		return fmt.Errorf("Pi supervisor host user/groups mismatch")
	}
	return nil
}
