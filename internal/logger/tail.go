package logger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

const tailReadBlockSize = 64 << 10

// TailLogFamily returns the newest complete lines from the canonical active
// log and its exact completed segments. maxBytes is one aggregate physical-read
// budget across the whole family, not a per-file allowance.
func TailLogFamily(activePath string, maxLines int, maxBytes int64) ([]byte, error) {
	return tailLogFamilyWithOps(activePath, maxLines, maxBytes, defaultRotationFileOps())
}

func tailLogFamilyWithOps(activePath string, maxLines int, maxBytes int64, ops rotationFileOps) ([]byte, error) {
	if activePath == "" {
		return nil, &os.PathError{Op: "open", Path: activePath, Err: os.ErrNotExist}
	}
	if maxLines <= 0 || maxBytes <= 0 {
		return nil, nil
	}

	remainingLines := maxLines
	remainingBytes := maxBytes
	chunks := make([][][]byte, 0, maxCompletedSegments+1)
	var activeInfo os.FileInfo
	found := false

	active, err := ops.openRead(activePath)
	if err == nil {
		found = true
		lines, consumed, info, readErr := readTailLines(active, remainingLines, remainingBytes)
		closeErr := active.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		activeInfo = info
		if len(lines) > 0 {
			chunks = append(chunks, lines)
			remainingLines -= len(lines)
		}
		remainingBytes -= consumed
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if remainingLines > 0 && remainingBytes > 0 {
		segments, _, scanErr := scanCompletedSegments(activePath, ops)
		if scanErr != nil {
			return nil, scanErr
		}
		sort.Slice(segments, func(i, j int) bool {
			return segments[i].sequence > segments[j].sequence
		})
		if len(segments) > maxCompletedSegments {
			segments = segments[:maxCompletedSegments]
		}

		for _, segment := range segments {
			if remainingLines <= 0 || remainingBytes <= 0 {
				break
			}
			if activeInfo != nil && os.SameFile(activeInfo, segment.info) {
				continue
			}

			beforeOpen, statErr := ops.lstat(segment.path)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return nil, statErr
			}
			if !beforeOpen.Mode().IsRegular() {
				continue
			}

			file, openErr := ops.openRead(segment.path)
			if openErr != nil {
				if os.IsNotExist(openErr) {
					continue
				}
				return nil, openErr
			}
			lines, consumed, openedInfo, readErr := readTailLines(file, remainingLines, remainingBytes)
			closeErr := file.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if !os.SameFile(beforeOpen, openedInfo) {
				return nil, fmt.Errorf("completed log segment changed while opening: %s", segment.path)
			}
			found = true
			if len(lines) > 0 {
				chunks = append(chunks, lines)
				remainingLines -= len(lines)
			}
			remainingBytes -= consumed
		}
	}

	if !found {
		return nil, &os.PathError{Op: "open", Path: activePath, Err: os.ErrNotExist}
	}

	var out bytes.Buffer
	for i := len(chunks) - 1; i >= 0; i-- {
		for _, line := range chunks[i] {
			_, _ = out.Write(line)
		}
	}
	return out.Bytes(), nil
}

func readTailLines(file *os.File, maxLines int, maxBytes int64) ([][]byte, int64, os.FileInfo, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, 0, nil, err
	}
	if info.Size() == 0 || maxLines <= 0 || maxBytes <= 0 {
		return nil, 0, info, nil
	}

	readLimit := min(info.Size(), maxBytes)
	maxInt := int64(^uint(0) >> 1)
	if readLimit > maxInt {
		readLimit = maxInt
	}

	position := info.Size()
	remaining := readLimit
	consumed := int64(0)
	newlineCount := 0
	newestFirst := make([][]byte, 0, int(readLimit/tailReadBlockSize)+1)
	for position > 0 && remaining > 0 {
		readSize := min(int64(tailReadBlockSize), position, remaining)
		offset := position - readSize
		block := make([]byte, int(readSize))
		n, readErr := file.ReadAt(block, offset)
		consumed += int64(n)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, consumed, info, readErr
		}
		if n != len(block) {
			return nil, consumed, info, io.ErrUnexpectedEOF
		}
		newestFirst = append(newestFirst, block)
		newlineCount += bytes.Count(block, []byte{'\n'})
		position = offset
		remaining -= readSize

		// When the window does not reach byte zero, one extra newline is
		// needed to prove the oldest selected record starts at a boundary.
		if newlineCount > maxLines {
			break
		}
	}

	data := make([]byte, 0, consumed)
	for i := len(newestFirst) - 1; i >= 0; i-- {
		data = append(data, newestFirst[i]...)
	}

	if position > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			return nil, consumed, info, nil
		}
		data = data[newline+1:]
	}
	// A record is published only after its terminating newline is visible.
	// This also prevents a crash-truncated legacy tail from being joined to the
	// first complete record of the next segment.
	if len(data) == 0 || data[len(data)-1] != '\n' {
		lastComplete := bytes.LastIndexByte(data, '\n')
		if lastComplete < 0 {
			return nil, consumed, info, nil
		}
		data = data[:lastComplete+1]
	}

	parts := bytes.SplitAfter(data, []byte{'\n'})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > maxLines {
		parts = parts[len(parts)-maxLines:]
	}
	return parts, consumed, info, nil
}
