package test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"perfprofiler/pkg/tools/profiling"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	PerfBufferSize = 4096
)

func CalculateMmapSize() int {
	pageSize := os.Getpagesize()
	pageCnt := PerfBufferSize / pageSize
	return (pageCnt + 32) * pageSize
}

// reflect of corresponding perf_event_header struct in perf_event.h
type PerfEventHeader struct {
	Type uint32
	Misc uint16
	Size uint16
}

type perfEventLost struct {
	Id   uint64
	Lost uint64
}

// When using perf_event_open() in sampled mode, asynchronous events (like counter overflow or PROT_EXEC mmap tracking)
// are logged into a ring-buffer.  This ring-buffer is created and accessed through mmap(2).
// The mmap size should be 1+2^n pages, where the first page is a metadata page (struct perf_event_mmap_page) that
// contains various bits of information such as where the ring-buffer head is.
type ShmmapRingBuffer struct {
	ptr       unsafe.Pointer
	shMemByte []byte
	tail      int
}

func NewMmapRingBuffer(ptr unsafe.Pointer, shmmap []byte) *ShmmapRingBuffer {
	meta_data := (*unix.PerfEventMmapPage)(ptr)
	res := &ShmmapRingBuffer{
		ptr:       ptr,
		shMemByte: shmmap,
		tail:      int(meta_data.Data_tail),
	}
	return res
}

// data_offset (since Linux 4.1) Contains the offset of the location in the mmap buffer where perf sample data begins.
func (b *ShmmapRingBuffer) getRingBufferStart() unsafe.Pointer {
	meta_data := (*unix.PerfEventMmapPage)(b.ptr)
	return unsafe.Pointer(&b.shMemByte[meta_data.Data_offset])
}

// data_size (since Linux 4.1) Contains the size of the perf sample region within the mmap buffer.
func (b *ShmmapRingBuffer) getRingBufferSize() int {
	meta_data := (*unix.PerfEventMmapPage)(b.ptr)
	return int(meta_data.Data_size)
}

// data_head points to the head of the data section.  The value continuously increases, it does not wrap.
// The value needs to be manually wrapped by the size of the mmap buffer before accessing the samples.
func (b *ShmmapRingBuffer) GetRingBufferHead() int {
	meta_data := (*unix.PerfEventMmapPage)(b.ptr)
	return int(meta_data.Data_head)
}

func (b *ShmmapRingBuffer) GetRingBufferTail() int {
	return b.tail
}

func (b *ShmmapRingBuffer) Read(size int) []byte {
	ringBufferSize := b.getRingBufferSize()
	ringBufferStart := b.getRingBufferStart()
	ringBufferEnd := uintptr(ringBufferStart) + uintptr(ringBufferSize)

	if size > ringBufferSize {
		size = ringBufferSize
	}

	res := make([]byte, size)
	tailPtr := unsafe.Pointer(uintptr(ringBufferStart) + uintptr(b.tail%ringBufferSize))

	if uintptr(tailPtr)+uintptr(size) <= uintptr(ringBufferEnd) {
		//non-overflow case
		memcpy(unsafe.Pointer(&res[0]), tailPtr, uintptr(size))
	} else {
		//Circular buffer
		//Read until the end
		dataToRead := int(uintptr(ringBufferEnd) - uintptr(tailPtr))
		memcpy(unsafe.Pointer(&res[0]), tailPtr, uintptr(dataToRead))
		//read over the size boundary
		memcpy(unsafe.Pointer(&res[dataToRead]), tailPtr, uintptr(size-dataToRead))
	}

	b.tail += size

	return res
}

func (b *ShmmapRingBuffer) RingBufferReadDone() {
	//Reset tail
	meta_data := (*unix.PerfEventMmapPage)(b.ptr)
	atomic.StoreUint64(&meta_data.Data_tail, uint64(b.tail))
}

func memcpy(dst, src unsafe.Pointer, count uintptr) {
	for i := uintptr(0); i < count; i++ {
		b := *(*byte)(unsafe.Pointer(uintptr(src) + i))
		*(*byte)(unsafe.Pointer(uintptr(dst) + i)) = b
	}
}

func readRingBuffer(ringBuffer *ShmmapRingBuffer) error {
	lostEvents := 0
	unknownEvents := 0

	for ringBuffer.GetRingBufferHead() != ringBuffer.GetRingBufferTail() {
		var header PerfEventHeader
		//1. Read header
		perfEventHeaderSize := binary.Size(PerfEventHeader{})
		headerData := ringBuffer.Read(perfEventHeaderSize)
		headerReader := bytes.NewReader(headerData)
		err := binary.Read(headerReader, binary.LittleEndian, &header)
		if err != nil {
			return fmt.Errorf("failed to read header from ringBuffer")
		}
		//2. Get the data part
		dataSize := int(header.Size) - perfEventHeaderSize
		data := ringBuffer.Read(dataSize)

		switch header.Type {
		//3. Read data to channel
		case unix.PERF_RECORD_SAMPLE:
			dataReader := bytes.NewReader(data)
			var PID uint32
			var TID uint32
			var Time uint64
			var nr uint64 // depth of the callchain
			binary.Read(dataReader, binary.LittleEndian, &PID)
			binary.Read(dataReader, binary.LittleEndian, &TID)
			binary.Read(dataReader, binary.LittleEndian, &Time)
			binary.Read(dataReader, binary.LittleEndian, &nr)
			fmt.Println("\nTime =", Time)
			fmt.Println("Pid, Tid =", PID, TID)
			fmt.Println("Stack:")

			var i, callchain uint64
			for i = 0; i < nr; i++ {
				binary.Read(dataReader, binary.LittleEndian, &callchain)
				fmt.Printf("[%d]   0x%x\n", i, callchain)
			}

		case unix.PERF_RECORD_LOST:
			var lost perfEventLost
			lostReader := bytes.NewReader(data)
			err := binary.Read(lostReader, binary.LittleEndian, &lost)
			if err != nil {
				err = fmt.Errorf("failed to read the lost data")
				return err
			}
			lostEvents += int(lost.Lost)

		default:
			unknownEvents++
		}
	}

	//https://github.com/jayanthvn/pure-gobpf/blob/e4dd30aaed9668e9507ea96bcbddb5d4e918efcf/pkg/ebpf_perf/perf.go#L235
	ringBuffer.RingBufferReadDone()

	return nil
}

func readRingBufferToSymobols(ringBuffer *ShmmapRingBuffer, kInfo *profiling.Info, pInfo *profiling.Info) error {
	lostEvents := 0
	unknownEvents := 0
	sampleEvents := 0

	for ringBuffer.GetRingBufferHead() != ringBuffer.GetRingBufferTail() {
		var header PerfEventHeader
		//1. Read header
		perfEventHeaderSize := binary.Size(PerfEventHeader{})
		headerData := ringBuffer.Read(perfEventHeaderSize)
		headerReader := bytes.NewReader(headerData)
		err := binary.Read(headerReader, binary.LittleEndian, &header)
		if err != nil {
			err = fmt.Errorf("failed to read header from ringBuffer")
			return err
		}
		//2. Get the data part
		dataSize := int(header.Size) - perfEventHeaderSize
		data := ringBuffer.Read(dataSize)

		switch header.Type {
		//3. Read data to channel
		case unix.PERF_RECORD_SAMPLE:
			sampleEvents++
			dataReader := bytes.NewReader(data)
			var PID uint32
			var TID uint32
			var Time uint64
			var nr uint64 // depth of the callchain
			binary.Read(dataReader, binary.LittleEndian, &PID)
			binary.Read(dataReader, binary.LittleEndian, &TID)
			binary.Read(dataReader, binary.LittleEndian, &Time)
			binary.Read(dataReader, binary.LittleEndian, &nr)
			fmt.Println("\n--------------------\nTime =", Time)
			fmt.Println("Pid, Tid =", PID, TID)

			var i, callchain uint64
			var symbolArray []uint64
			for i = 0; i < nr; i++ {
				binary.Read(dataReader, binary.LittleEndian, &callchain)
				symbolArray = append(symbolArray, callchain)
			}
			fmt.Println("Stack depth =", len((symbolArray)))
			symbols := findSymbols(symbolArray, kInfo, pInfo, "[MISSING]")
			if len(symbols) == 0 {
				return nil
			}
			for _, s := range symbols {
				fmt.Println(s)
			}

		case unix.PERF_RECORD_LOST:
			var lost perfEventLost
			lostReader := bytes.NewReader(data)
			err := binary.Read(lostReader, binary.LittleEndian, &lost)
			if err != nil {
				err = fmt.Errorf("failed to read the lost data")
				return err
			}
			lostEvents += int(lost.Lost)

		default:
			unknownEvents++
		}
	}

	//https://github.com/jayanthvn/pure-gobpf/blob/e4dd30aaed9668e9507ea96bcbddb5d4e918efcf/pkg/ebpf_perf/perf.go#L235
	ringBuffer.RingBufferReadDone()

	fmt.Println("Statistics: sampleEvent, unknownEvent, lostEvent", sampleEvents, unknownEvents, lostEvents)

	return nil
}

// FindSymbols from address list, if could not found symbol name then append default symbol to array
func findSymbols(addresses []uint64, kInfo *profiling.Info, pInfo *profiling.Info, defaultSymbol string) []string {
	if len(addresses) == 0 {
		return nil
	}
	result := make([]string, 0)
	for _, addr := range addresses {
		if addr <= 0 {
			continue
		}
		s := kInfo.FindSymbolName(addr)
		if s == "" {
			s = pInfo.FindSymbolName(addr)
		}
		if s == "" {
			// s = defaultSymbol
			s = fmt.Sprintf("0x%x", addr)
		}
		result = append(result, s)
	}
	return result
}
