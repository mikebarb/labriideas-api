package media

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
)

// CalculateAudioHash generates an MD5 hash of ONLY the audio frames in an MP3,
// ignoring all ID3v2 (front) and ID3v1 (back) metadata tags.
func CalculateAudioHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// 1. Determine the total file size
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	totalSize := info.Size()

	// 2. Calculate where to START reading (Skip ID3v2 header at the front)
	startOffset := int64(0)
	header := make([]byte, 10)
	if _, err := io.ReadFull(file, header); err == nil {
		if string(header[:3]) == "ID3" {
			// ID3v2 size is encoded in bytes 6-9 using synchsafe integers
			size := int64(header[6])<<21 | int64(header[7])<<14 | int64(header[8])<<7 | int64(header[9])
			startOffset = size + 10 // 10 bytes for the ID3 header itself
		}
	}

	// 3. Calculate where to STOP reading (Skip ID3v1 tag at the very end)
	endOffset := totalSize
	if totalSize > 128 {
		// ID3v1 tag is always 128 bytes at the end, starting with "TAG"
		var tail [3]byte
		file.ReadAt(tail[:], totalSize-128)
		if string(tail[:]) == "TAG" {
			endOffset = totalSize - 128
		}
	}

	// 4. Seek to the start of the audio data
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return "", err
	}

	// 5. Hash the audio data using a LimitReader to stop before the ID3v1 tag
	hash := md5.New()
	limitReader := io.LimitReader(file, endOffset-startOffset)
	if _, err := io.Copy(hash, limitReader); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}
