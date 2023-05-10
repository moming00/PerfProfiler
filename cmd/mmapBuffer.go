package cmd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

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
			err = fmt.Errorf("Failed to read header from ringBuffer")
			return err
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
				err = fmt.Errorf("Failed to read the lost data")
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

func (b *ShmmapRingBuffer) getRingBufferStart() unsafe.Pointer {
	meta_data := (*unix.PerfEventMmapPage)(b.ptr)
	return unsafe.Pointer(&b.shMemByte[meta_data.Data_offset])
}

func (b *ShmmapRingBuffer) getRingBufferSize() int {
	meta_data := (*unix.PerfEventMmapPage)(b.ptr)
	return int(meta_data.Data_size)
}

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
