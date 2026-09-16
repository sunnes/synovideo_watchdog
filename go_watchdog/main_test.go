package main

import (
	"encoding/binary"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gen2brain/h265/hevc"
)

func TestRunWatchdogReturnsFailureAfterRetries(t *testing.T) {
	checks := 0
	err := runWatchdog(2, func() error {
		checks++
		return fmt.Errorf("capture failed")
	})
	if err == nil || !strings.Contains(err.Error(), "screenshot check failed after 2 attempts") || checks != 2 {
		t.Fatalf("err=%v checks=%d; want failure, 2", err, checks)
	}
}

func TestRunWatchdogStopsAfterSuccess(t *testing.T) {
	checks := 0
	err := runWatchdog(3, func() error {
		checks++
		if checks == 2 {
			return nil
		}
		return fmt.Errorf("capture failed")
	})
	if err != nil || checks != 2 {
		t.Fatalf("err=%v checks=%d; want nil, 2", err, checks)
	}
}

func TestUnwrapFrame(t *testing.T) {
	header := []byte("mediaType=1&sequence=42")
	payload := []byte("video-data")
	message := make([]byte, 4+len(header)+len(payload))
	binary.BigEndian.PutUint32(message[:4], uint32(len(header)))
	copy(message[4:], header)
	copy(message[4+len(header):], payload)

	gotHeader, gotPayload := unwrapFrame(message)
	if gotHeader["mediaType"] != "1" || string(gotPayload) != string(payload) {
		t.Fatalf("unwrapFrame() header=%v payload=%q", gotHeader, gotPayload)
	}
}

func TestPythonAcceptsPayload(t *testing.T) {
	if !pythonAcceptsPayload(map[string]string{"mediaType": "1"}, []byte("data")) {
		t.Fatal("mediaType=1 payload was not accepted")
	}
	if pythonAcceptsPayload(map[string]string{"mediaType": "2"}, []byte("data")) {
		t.Fatal("non-video media type was accepted")
	}
	if !pythonAcceptsPayload(nil, []byte("moof-data")) {
		t.Fatal("headerless payload was not accepted")
	}
	if pythonAcceptsPayload(map[string]string{"vdoCodec": "H265", "vdoExtra": "99"}, []byte{0x40, 0x01}) {
		t.Fatal("unframed H.265 parameter payload was accepted")
	}
}

func TestFrameDecoderUsesPythonTransportDetection(t *testing.T) {
	decoder := frameDecoder{}
	ok, err := decoder.feed([]byte{0x40, 0x01, 0xaa})
	if err != nil || ok || decoder.backend != transportUnknown {
		t.Fatalf("unframed payload: ok=%v err=%v backend=%v", ok, err, decoder.backend)
	}

	ok, err = decoder.feed([]byte("mdatdata"))
	if err != nil || ok || decoder.backend != transportFMP4 {
		t.Fatalf("fMP4 payload: ok=%v err=%v backend=%v", ok, err, decoder.backend)
	}
}

func TestHandlePictureSavesDebugPNG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.png")
	decoder := frameDecoder{minBytes: 1, debugFile: path}
	picture := &hevc.Picture{
		Width: 2, Height: 2, CropW: 2, CropH: 2,
		ChromaFormat: 1, BitDepth: 8, BitDepthC: 8,
		Y: []byte{16, 32, 64, 128}, Cb: []byte{128}, Cr: []byte{128},
		StrideY: 2, StrideC: 1,
	}
	ok, err := decoder.handlePicture(picture)
	if err != nil || !ok {
		t.Fatalf("handlePicture() ok=%v err=%v", ok, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	img, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 2 || img.Bounds().Dy() != 2 {
		t.Fatalf("debug PNG bounds = %v", img.Bounds())
	}
}
