package test

import (
	"context"
	"fmt"
	"perfprofiler/pkg/process/api"
	"perfprofiler/pkg/process/finders"
	"perfprofiler/pkg/process/finders/scanner"
	"perfprofiler/pkg/profiling/task/base"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestCallChain(t *testing.T) {
	processID := 455024 // replace with the actual process ID

	attr := &unix.PerfEventAttr{
		Type:        unix.PERF_TYPE_SOFTWARE,
		Config:      unix.PERF_COUNT_SW_CPU_CLOCK,
		Sample:      10,
		Wakeup:      1,
		Sample_type: unix.PERF_SAMPLE_TIME | unix.PERF_SAMPLE_TID | unix.PERF_SAMPLE_CALLCHAIN,
	}

	fd, err := unix.PerfEventOpen(attr, processID, -1, -1, 0)
	if err != nil {
		fmt.Println("Error opening perf event:", err)
		return
	}
	defer syscall.Close(fd)

	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		fmt.Println("Error SetNonblock:", err)
	}

	mmapBuf, err := unix.Mmap(fd, 0, CalculateMmapSize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		fmt.Println("error mmaping perf event:", err)
		return
	}
	defer unix.Munmap(mmapBuf)

	ringbuffer := NewMmapRingBuffer(unsafe.Pointer(&mmapBuf[0]), mmapBuf)

	// the offset of next sample

	err = unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0)
	if err != nil {
		fmt.Println("Error enabling perf event:", err)
		return
	}
	defer unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_DISABLE, 0)

	// Create a PollFd struct for the file descriptor
	pfd := unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}

	// Wait for samples and print call chains
	for {
		// Call Poll with a timeout of 1 second
		n, err := unix.Poll([]unix.PollFd{pfd}, 10000)
		if err != nil {
			panic(err)
		}
		// Check the result of the poll
		if n == 0 {
			fmt.Println("Timeout")
		} else if n < 0 {
			panic("Poll error")
		} else {
			fmt.Printf("Events: %d\n", pfd.Revents)
		}

		readRingBuffer(ringbuffer)
	}
}

//https://github.com/ApsaraDB/PolarDB-NodeAgent/blob/3f388d4a4e5816a39e5efe6c6d50ada4957f23e5/plugins/perf/collector/profiler.go#L100

func TestNewCT(t *testing.T) {
	conf := &base.TaskConfig{
		OnCPU: &base.OnCPUConfig{
			Period: "10ms",
		},
	}
	r, e := NewRunner(conf, nil)
	assert.NoError(t, e)

	task := &base.ProfilingTask{
		TaskID:          "testmain",
		ProcessIDList:   []string{"471295"},
		UpdateTime:      time.Now().Unix(),
		StartTime:       time.Now().Unix(),
		TargetType:      base.TargetTypeOnCPU,
		TriggerType:     "",
		ExtensionConfig: &base.ExtensionConfig{},
	}
	assert.NoError(t, e)

	finder := &scanner.ProcessFinder{}
	finderConf := &scanner.Config{
		Period:   "30s",
		ScanMode: scanner.Regex,
		Agent:    nil,
		RegexFinders: []*scanner.RegexFinder{{
			MatchCommandRegex: "testmain",
			Layer:             "OS_LINUX",
			ServiceName:       "testmain",
			InstanceName:      "{{.Rover.HostIPV4 \"p3p2\"}}",
			ProcessName:       "testmain",
		}},
	}
	finder.Init(context.Background(), finderConf, nil)
	pes, e := finder.FindProcesses()
	assert.NoError(t, e)

	processes := make([]api.ProcessInterface, 0)
	for _, pesi := range pes {
		p := finders.NewProcessContext(finder.DetectType(), pesi)
		processes = append(processes, p)
	}

	if e = r.Init(task, processes); e != nil {
		log.Fatal(fmt.Sprintf("error calling Run - %v", e))
	}

	e = r.Run(context.Background(), func() {})
	if e != nil {
		log.Fatal(fmt.Sprintf("error calling Run - %v", e))
	}

	for {
		data, e := r.FlushData()
		assert.NoError(t, e)

		if len(data) == 0 {
			continue
		}
	}
}
