package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaximumImagesPerTurn bounds browser, memory, and provider work per turn.
	MaximumImagesPerTurn = 4
	// MaximumImageBytes is the decoded size limit for one input or output image.
	MaximumImageBytes = 8 << 20
)

var supportedImageTypes = map[string]string{
	"image/gif":  ".gif",
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// Image is an ephemeral base64-encoded image attachment. Image bytes are never
// persisted by RepoKarta beyond the active provider turn.
type Image struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ValidateImages enforces the provider-neutral attachment contract.
func ValidateImages(images []Image) error {
	if len(images) > MaximumImagesPerTurn {
		return fmt.Errorf("at most %d images can be attached to one turn", MaximumImagesPerTurn)
	}
	for index, image := range images {
		if _, err := DecodeImage(image); err != nil {
			return fmt.Errorf("image %d: %w", index+1, err)
		}
	}
	return nil
}

// DecodeImage validates and decodes one image.
func DecodeImage(image Image) ([]byte, error) {
	mediaType := strings.ToLower(strings.TrimSpace(image.MediaType))
	if _, ok := supportedImageTypes[mediaType]; !ok {
		return nil, fmt.Errorf("unsupported media type %q", image.MediaType)
	}
	if image.Data == "" {
		return nil, errors.New("image data is required")
	}
	if base64.StdEncoding.DecodedLen(len(image.Data)) > MaximumImageBytes {
		return nil, fmt.Errorf("image exceeds the %d MiB limit", MaximumImageBytes>>20)
	}
	decoded, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil {
		return nil, errors.New("image data is not valid base64")
	}
	if len(decoded) == 0 {
		return nil, errors.New("image data is empty")
	}
	if len(decoded) > MaximumImageBytes {
		return nil, fmt.Errorf("image exceeds the %d MiB limit", MaximumImageBytes>>20)
	}
	detected := http.DetectContentType(decoded)
	if detected != mediaType && !(mediaType == "image/jpeg" && detected == "image/jpeg") {
		return nil, fmt.Errorf("declared media type %q does not match file content %q", mediaType, detected)
	}
	return decoded, nil
}

// ImageFromFile loads a generated image into the provider-neutral event shape.
func ImageFromFile(path string) (Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return Image{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaximumImageBytes+1))
	if err != nil {
		return Image{}, err
	}
	if len(data) > MaximumImageBytes {
		return Image{}, fmt.Errorf("generated image exceeds the %d MiB limit", MaximumImageBytes>>20)
	}
	mediaType := http.DetectContentType(data)
	if _, ok := supportedImageTypes[mediaType]; !ok {
		return Image{}, fmt.Errorf("generated file has unsupported media type %q", mediaType)
	}
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "generated" + supportedImageTypes[mediaType]
	}
	return Image{
		Name:      name,
		MediaType: mediaType,
		Data:      base64.StdEncoding.EncodeToString(data),
	}, nil
}

// AttachmentStore owns temporary provider-readable copies for one session.
type AttachmentStore struct {
	directory string
}

// NewAttachmentStore creates a private temporary image directory.
func NewAttachmentStore() (*AttachmentStore, error) {
	directory, err := os.MkdirTemp("", "repokarta-chat-images-*")
	if err != nil {
		return nil, err
	}
	return &AttachmentStore{directory: directory}, nil
}

// Directory returns the provider allow-listed attachment directory.
func (s *AttachmentStore) Directory() string {
	if s == nil {
		return ""
	}
	return s.directory
}

// Write creates temporary files for a single turn.
func (s *AttachmentStore) Write(images []Image) ([]string, error) {
	if s == nil {
		return nil, errors.New("attachment store is not configured")
	}
	if err := ValidateImages(images); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(images))
	for _, image := range images {
		decoded, err := DecodeImage(image)
		if err != nil {
			s.Remove(paths)
			return nil, err
		}
		extension := supportedImageTypes[strings.ToLower(strings.TrimSpace(image.MediaType))]
		file, err := os.CreateTemp(s.directory, "image-*"+extension)
		if err != nil {
			s.Remove(paths)
			return nil, err
		}
		path := file.Name()
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			s.Remove(paths)
			return nil, err
		}
		_, err = file.Write(decoded)
		closeErr := file.Close()
		if err != nil {
			_ = os.Remove(path)
			s.Remove(paths)
			return nil, err
		}
		if closeErr != nil {
			_ = os.Remove(path)
			s.Remove(paths)
			return nil, closeErr
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// Remove deletes files created for a completed turn.
func (s *AttachmentStore) Remove(paths []string) {
	if s == nil {
		return
	}
	for _, path := range paths {
		if filepath.Dir(path) == s.directory {
			_ = os.Remove(path)
		}
	}
}

// Close removes every remaining attachment for the session.
func (s *AttachmentStore) Close() error {
	if s == nil || s.directory == "" {
		return nil
	}
	return os.RemoveAll(s.directory)
}
