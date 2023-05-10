package cmd

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	perfBufferSize = 4096
)

func calculateMmapSize() int {
	pageSize := os.Getpagesize()
	pageCnt := perfBufferSize / pageSize
	return (pageCnt + 2) * pageSize
}

func TestCallChain(t *testing.T) {
	processID := 244634 // replace with the actual process ID

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

	mmapBuf, err := unix.Mmap(fd, 0, calculateMmapSize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
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
		n, err := unix.Poll([]unix.PollFd{pfd}, 1000)
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
