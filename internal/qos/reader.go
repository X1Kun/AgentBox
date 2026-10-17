package qos

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

type Source struct {
	Name, Path string
	Cgroup     bool
}
type identity struct{ Device, Inode uint64 }
type Reading struct {
	At          time.Time
	Duration    time.Duration
	CPU, Memory Pressure
	Resources   cgroup.Stats
	Identity    identity
}

type Reader interface{ Read(Source) (Reading, error) }
type FileReader struct{}

func (FileReader) Read(source Source) (Reading, error) {
	begin := time.Now()
	var reading Reading
	before, err := os.Stat(source.Path)
	if err != nil {
		return reading, err
	}
	if !before.IsDir() {
		return reading, fmt.Errorf("source is not a directory")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok {
		return reading, fmt.Errorf("source identity unavailable")
	}
	reading.Identity = identity{uint64(stat.Dev), stat.Ino}
	read := func(name string) (string, error) { return readFile(filepath.Join(source.Path, name)) }
	cpuName, memoryName := "cpu", "memory"
	if source.Cgroup {
		cpuName, memoryName = "cpu.pressure", "memory.pressure"
	}
	data, err := read(cpuName)
	if err != nil {
		return reading, err
	}
	if reading.CPU, err = ParsePressure(data); err != nil {
		return reading, fmt.Errorf("%s: %w", cpuName, err)
	}
	if !source.Cgroup {
		reading.CPU.HasFull = false
	} // system-wide CPU full is undefined
	data, err = read(memoryName)
	if err != nil {
		return reading, err
	}
	if reading.Memory, err = ParsePressure(data); err != nil {
		return reading, fmt.Errorf("%s: %w", memoryName, err)
	}
	if !reading.Memory.HasFull {
		return reading, fmt.Errorf("%s: missing full line", memoryName)
	}
	if source.Cgroup {
		if reading.Resources, err = cgroup.ReadStats(read); err != nil {
			return reading, err
		}
	}
	after, err := os.Stat(source.Path)
	if err != nil {
		return reading, err
	}
	if !os.SameFile(before, after) {
		return reading, fmt.Errorf("source changed during collection")
	}
	reading.At = time.Now()
	reading.Duration = reading.At.Sub(begin)
	return reading, nil
}

func readFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(data) > 65536 {
		return "", fmt.Errorf("%s exceeds observation size limit", filepath.Base(path))
	}
	return string(data), nil
}
