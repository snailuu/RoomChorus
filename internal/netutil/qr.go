package netutil

import (
	"github.com/skip2/go-qrcode"
)

// GeneratePNG encodes content as a PNG QR code of the requested pixel size.
func GeneratePNG(content string, size int) ([]byte, error) {
	if size <= 0 {
		size = 320
	}
	return qrcode.Encode(content, qrcode.Medium, size)
}

// TerminalQR renders a compact UTF-8 QR code suitable for terminal display.
func TerminalQR(content string) string {
	qr, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return ""
	}
	return qr.ToSmallString(false)
}
