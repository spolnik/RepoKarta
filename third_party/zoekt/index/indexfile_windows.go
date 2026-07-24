// Copyright 2016 Google Inc. All rights reserved.
// Copyright 2026 RepoKarta contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package index

import (
	"fmt"
	"log"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type mmapedIndexFile struct {
	name    string
	size    uint32
	mapping windows.Handle
	address uintptr
	data    []byte
}

func (f *mmapedIndexFile) Read(off, sz uint32) ([]byte, error) {
	if off > off+sz || off+sz > uint32(len(f.data)) {
		return nil, fmt.Errorf("out of bounds: %d, len %d, name %s", off+sz, len(f.data), f.name)
	}
	return f.data[off : off+sz], nil
}

func (f *mmapedIndexFile) Name() string {
	return f.name
}

func (f *mmapedIndexFile) Size() (uint32, error) {
	return f.size, nil
}

func (f *mmapedIndexFile) Close() {
	f.data = nil
	if f.address != 0 {
		if err := windows.UnmapViewOfFile(f.address); err != nil {
			log.Printf("WARN failed to UnmapViewOfFile %s: %v", f.name, err)
		}
		f.address = 0
	}
	if f.mapping != 0 {
		if err := windows.CloseHandle(f.mapping); err != nil {
			log.Printf("WARN failed to CloseHandle for %s: %v", f.name, err)
		}
		f.mapping = 0
	}
}

// NewIndexFile returns a memory-mapped Windows index file. The index file
// takes ownership of the passed file and closes it after creating the mapping.
func NewIndexFile(f *os.File) (IndexFile, error) {
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size >= math.MaxUint32 {
		return nil, fmt.Errorf("file %s too large: %d", f.Name(), size)
	}

	result := &mmapedIndexFile{name: f.Name(), size: uint32(size)}
	if size == 0 {
		return result, nil
	}

	result.mapping, err = windows.CreateFileMapping(
		windows.Handle(f.Fd()),
		nil,
		windows.PAGE_READONLY,
		0,
		0,
		nil,
	)
	if err != nil {
		return nil, err
	}
	result.address, err = windows.MapViewOfFile(
		result.mapping,
		windows.FILE_MAP_READ,
		0,
		0,
		uintptr(size),
	)
	if err != nil {
		windows.CloseHandle(result.mapping)
		return nil, err
	}
	result.data = unsafe.Slice((*byte)(unsafe.Pointer(result.address)), int(size))
	return result, nil
}
