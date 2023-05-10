package test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"perfprofiler/pkg/logger"
	"perfprofiler/pkg/module"
	"perfprofiler/pkg/process/api"
	"perfprofiler/pkg/profiling/task/base"
	"perfprofiler/pkg/tools/process"
	"perfprofiler/pkg/tools/profiling"

	"golang.org/x/sys/unix"

	v3 "skywalking.apache.org/repo/goapi/collect/ebpf/profiling/v3"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
// nolint
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -no-global-types -target bpfel -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf $REPO_ROOT/bpf/profiling/oncpu.c -- -I$REPO_ROOT/bpf/include

var log = logger.GetLogger("profiling", "task", "oncpu")

type Event struct {
	UserStackID   uint32
	KernelStackID uint32
}

type PerfEvent struct {
	sync.Mutex
	fd   unix.PollFd
	rbuf *ShmmapRingBuffer
}
type Runner struct {
	base             *base.Runner
	pid              int32
	processProfiling *profiling.Info
	kernelProfiling  *profiling.Info
	dumpFrequency    int64

	// runtime
	stackCounter    map[Event]uint32
	flushDataNotify context.CancelFunc
	stopChan        chan bool
	events          []*PerfEvent
}

func NewRunner(config *base.TaskConfig, moduleMgr *module.Manager) (base.ProfileTaskRunner, error) {
	if config.OnCPU.Period == "" {
		return nil, fmt.Errorf("please provide the ON_CPU dump period")
	}
	dumpPeriod, err := time.ParseDuration(config.OnCPU.Period)
	if err != nil {
		return nil, fmt.Errorf("the ON_CPU dump period format not right, current value: %s", config.OnCPU.Period)
	}
	if dumpPeriod < time.Millisecond {
		return nil, fmt.Errorf("the ON_CPU dump period could not be smaller than 1ms")
	}
	return &Runner{
		base:          base.NewBaseRunner(),
		dumpFrequency: time.Second.Milliseconds() / dumpPeriod.Milliseconds(),
	}, nil
}

func (r *Runner) Init(task *base.ProfilingTask, processes []api.ProcessInterface) error {
	if len(processes) != 1 {
		return fmt.Errorf("the processes count must be 1, current is: %d", len(processes))
	}
	curProcess := processes[0]
	r.pid = curProcess.Pid()
	// process profiling stat
	if r.processProfiling = curProcess.ProfilingStat(); r.processProfiling == nil {
		return fmt.Errorf("this process could not be profiling")
	}
	// kernel profiling stat
	kernelProfiling, err := process.KernelFileProfilingStat()
	if err != nil {
		log.Warnf("could not analyze kernel profiling stats: %v", err)
	}
	r.kernelProfiling = kernelProfiling
	r.stackCounter = make(map[Event]uint32)
	r.stopChan = make(chan bool, 1)
	return nil
}

func (r *Runner) Run(ctx context.Context, notify base.ProfilingRunningSuccessNotify) error {
	// opened perf events
	err := r.openPerfEvent()
	if err != nil {
		return err
	}

	return nil
}

func (r *Runner) openPerfEvent() error {
	value := 100
	if data, err := os.ReadFile("/proc/sys/kernel/perf_event_max_stack"); err == nil {
		i := 0
		for i = 0; i < len(data); i++ {
			if data[i] > '9' || data[i] < '0' {
				break
			}
		}
		if num, err := strconv.Atoi(string(data[:i])); err == nil {
			if num < 100 {
				value = int(num)
			}
		}
	}

	eventAttr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		// A "sampling" event is one that generates an overflow	notification every N events, where N
		// is given by .  sample_freq can be used if you wish to use frequency rather than period.
		// When an overflow occurs, requested data is recorded in the mmap buffer, POLL_IN is indicated.
		Sample: 100,
		// Enable sample_freq instead of sample_period, Hertz (samples per second)
		Bits:             unix.PerfBitFreq,
		Sample_max_stack: uint16(value),
		// Generate an overflow notification every wakeup_events events. If set PerfBitWatermark, have
		// an overflow notification happen when we cross the wakeup_watermark boundary.
		Wakeup: 1,
		// The sample_type field controls what data is recorded on each overflow.
		Sample_type: unix.PERF_SAMPLE_TIME | unix.PERF_SAMPLE_TID | unix.PERF_SAMPLE_CALLCHAIN,
	}

	files, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", r.pid))
	if err != nil {
		return err
	}

	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		if tid, e := strconv.Atoi(file.Name()); e == nil {
			fd, err := unix.PerfEventOpen(eventAttr, tid, -1, -1, 0)
			if err != nil {
				return err
			}
			if err := unix.SetNonblock(fd, true); err != nil {
				unix.Close(fd)
				fmt.Println("Error SetNonblock:", err)
			}
			// enable perf event
			if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
				return err
			}
			// When the mapping is PROT_WRITE, the kernel will not overwrite unread data.
			// The PerfEventMmapPage.data_tail value should be written by user space to reflect the last read data.
			mmapBuf, err := unix.Mmap(fd, 0, CalculateMmapSize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
			if err != nil {
				return err
			}

			rbuf := NewMmapRingBuffer(unsafe.Pointer(&mmapBuf[0]), mmapBuf)
			// Create a PollFd struct for the file descriptor
			pfd := unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
			r.events = append(r.events, &PerfEvent{fd: pfd, rbuf: rbuf})
		}
	}

	return err
}

func (r *Runner) Stop() error {
	return nil
}

func (r *Runner) FlushData() ([]*v3.EBPFProfilingData, error) {
	var fds []unix.PollFd
	for _, evt := range r.events {
		fds = append(fds, evt.fd)
	}

	// Wait for samples and print call chains
	{
		// Call Poll with a timeout of 10 second
		n, err := unix.Poll(fds, 10000)
		if err != nil {
			if err == syscall.EINTR {
				return nil, nil
			}
			panic(err)
		}
		// Check the result of the poll
		if n == 0 {
			fmt.Println("Timeout")
		} else if n < 0 {
			panic("Poll error")
		} else {
			for _, evt := range r.events {
				if evt.fd.Events&unix.POLLIN != 0 {
					evt.Lock()
					readRingBufferToSymobols(evt.rbuf, r.kernelProfiling, r.processProfiling)
					evt.Unlock()
				}
			}
		}

	}

	// close the flush data notify if exists
	if r.flushDataNotify != nil {
		r.flushDataNotify()
	}

	return nil, nil
}

// func (r *Runner) closePerfEvent(fd int) error {
// 	if fd <= 0 {
// 		return nil
// 	}
// 	var result error
// 	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_DISABLE, 0); err != nil {
// 		result = multierror.Append(result, fmt.Errorf("closing perf event reader: %s", err))
// 	}
// 	return result

// 	// unix.Munmap(mmapBuf)
// }
