//go:build linux

package rtspcamera

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Minimal pure-Go Video4Linux2 streaming capture (mmap I/O). Struct layouts
// mirror <linux/videodev2.h>; Go's natural alignment and unix.Timeval give the
// same sizes as the C structs on 32- and 64-bit targets.

const (
	v4l2BufTypeVideoCapture = 1
	v4l2MemoryMmap          = 1
	v4l2FieldAny            = 0
	v4l2CapVideoCapture     = 0x00000001
	v4l2CapStreaming        = 0x04000000
	v4l2CapDeviceCaps       = 0x80000000
	v4l2BufFlagError        = 0x00000040

	v4l2BufferCount = 4
	// Raw YUYV frames are large; keeping more than ~10/s only burns memory bandwidth.
	v4l2MinRawFrameInterval = 100 * time.Millisecond
	v4l2PollTimeoutMs       = 500
	v4l2FrameTimeout        = 10 * time.Second
	v4l2JPEGQuality         = 85
)

var (
	v4l2PixFmtMJPEG = fourcc('M', 'J', 'P', 'G')
	v4l2PixFmtJPEG  = fourcc('J', 'P', 'E', 'G')
	v4l2PixFmtYUYV  = fourcc('Y', 'U', 'Y', 'V')
)

type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	PixelFormat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	YcbcrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

// The fmt union contains pointers (struct v4l2_window), so it is pointer-aligned.
type v4l2Format struct {
	Type uint32
	_    [unsafe.Sizeof(uintptr(0)) - 4]byte
	Pix  v4l2PixFormat
	_    [200 - unsafe.Sizeof(v4l2PixFormat{})]byte
}

type v4l2RequestBuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	Reserved     [3]uint8
}

type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	Timestamp unix.Timeval
	Timecode  [16]byte
	Sequence  uint32
	Memory    uint32
	M         uintptr // union { offset; userptr; planes; fd }
	Length    uint32
	Reserved2 uint32
	RequestFD uint32
}

// offset reads the __u32 member of the m union regardless of endianness.
func (b *v4l2Buffer) offset() uint32 { return *(*uint32)(unsafe.Pointer(&b.M)) }

type v4l2StreamParm struct {
	Type    uint32
	Capture struct {
		Capability   uint32
		CaptureMode  uint32
		TimePerFrame struct{ Numerator, Denominator uint32 }
		ExtendedMode uint32
		ReadBuffers  uint32
		Reserved     [4]uint32
	}
	_ [200 - 40]byte
}

var (
	vidiocQueryCap  = iocR('V', 0, unsafe.Sizeof(v4l2Capability{}))
	vidiocGFmt      = iocRW('V', 4, unsafe.Sizeof(v4l2Format{}))
	vidiocSFmt      = iocRW('V', 5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqBufs   = iocRW('V', 8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQueryBuf  = iocRW('V', 9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQBuf      = iocRW('V', 15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDQBuf     = iocRW('V', 17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamOn  = iocW('V', 18, unsafe.Sizeof(int32(0)))
	vidiocStreamOff = iocW('V', 19, unsafe.Sizeof(int32(0)))
	vidiocSParm     = iocRW('V', 22, unsafe.Sizeof(v4l2StreamParm{}))
)

// Compile-time ABI checks against <linux/videodev2.h>: a size drift makes one
// of the constant array lengths overflow and breaks the build for that target.
const v4l2Is64 = unsafe.Sizeof(uintptr(0)) / 8 // 1 on 64-bit, 0 on 32-bit

var (
	_ [unsafe.Sizeof(v4l2Capability{}) - 104]byte
	_ [104 - unsafe.Sizeof(v4l2Capability{})]byte
	_ [unsafe.Sizeof(v4l2Format{}) - (204 + 4*v4l2Is64)]byte
	_ [(204 + 4*v4l2Is64) - unsafe.Sizeof(v4l2Format{})]byte
	_ [unsafe.Sizeof(v4l2RequestBuffers{}) - 20]byte
	_ [20 - unsafe.Sizeof(v4l2RequestBuffers{})]byte
	_ [unsafe.Sizeof(v4l2Buffer{}) - (68 + 20*v4l2Is64)]byte
	_ [(68 + 20*v4l2Is64) - unsafe.Sizeof(v4l2Buffer{})]byte
	_ [unsafe.Sizeof(v4l2StreamParm{}) - 204]byte
	_ [204 - unsafe.Sizeof(v4l2StreamParm{})]byte
)

func fourcc(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

func fourccString(v uint32) string {
	return string([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}

// Generic _IOC encoding (x86, ARM, RISC-V).
func ioc(dir, typ, nr, size uintptr) uintptr { return dir<<30 | size<<16 | typ<<8 | nr }
func iocR(typ, nr, size uintptr) uintptr     { return ioc(2, typ, nr, size) }
func iocW(typ, nr, size uintptr) uintptr     { return ioc(1, typ, nr, size) }
func iocRW(typ, nr, size uintptr) uintptr    { return ioc(3, typ, nr, size) }

func v4l2Ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

// v4l2Device is an open, configured capture device with mmap'ed buffers.
type v4l2Device struct {
	fd        int
	buffers   [][]byte
	pix       v4l2PixFormat
	card      string
	streaming bool
}

func openV4L2Device(opts v4l2Options) (*v4l2Device, error) {
	fd, err := unix.Open(opts.Device, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", opts.Device, err)
	}
	d := &v4l2Device{fd: fd}
	if err := d.configure(opts); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *v4l2Device) configure(opts v4l2Options) error {
	var capability v4l2Capability
	if err := v4l2Ioctl(d.fd, vidiocQueryCap, unsafe.Pointer(&capability)); err != nil {
		return fmt.Errorf("%s is not a V4L2 device: %w", opts.Device, err)
	}
	caps := capability.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = capability.DeviceCaps
	}
	if caps&v4l2CapVideoCapture == 0 || caps&v4l2CapStreaming == 0 {
		return fmt.Errorf("%s does not support video capture streaming (metadata node? try another /dev/videoN)", opts.Device)
	}
	d.card = cString(capability.Card[:])

	var current v4l2Format
	current.Type = v4l2BufTypeVideoCapture
	if err := v4l2Ioctl(d.fd, vidiocGFmt, unsafe.Pointer(&current)); err != nil {
		return fmt.Errorf("get format: %w", err)
	}
	width, height := current.Pix.Width, current.Pix.Height
	if opts.Width > 0 {
		width, height = uint32(opts.Width), uint32(opts.Height)
	}

	var candidates []uint32
	switch opts.Format {
	case V4L2FormatMJPEG:
		candidates = []uint32{v4l2PixFmtMJPEG, v4l2PixFmtJPEG}
	case V4L2FormatYUYV:
		candidates = []uint32{v4l2PixFmtYUYV}
	default:
		candidates = []uint32{v4l2PixFmtMJPEG, v4l2PixFmtJPEG, v4l2PixFmtYUYV}
	}
	accepted := false
	for _, pf := range candidates {
		var f v4l2Format
		f.Type = v4l2BufTypeVideoCapture
		f.Pix.Width, f.Pix.Height, f.Pix.PixelFormat, f.Pix.Field = width, height, pf, v4l2FieldAny
		if err := v4l2Ioctl(d.fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
			if errors.Is(err, unix.EBUSY) {
				return fmt.Errorf("%s is busy (in use by another process)", opts.Device)
			}
			continue
		}
		// Drivers silently substitute an unsupported pixel format.
		if f.Pix.PixelFormat == pf {
			d.pix, accepted = f.Pix, true
			break
		}
	}
	if !accepted {
		return fmt.Errorf("%s supports none of the requested pixel formats (%s); supported here: MJPEG, YUYV", opts.Device, opts.Format)
	}
	if d.pix.PixelFormat == v4l2PixFmtYUYV && (d.pix.Width%2 != 0 || d.pix.BytesPerLine < d.pix.Width*2) {
		return fmt.Errorf("%s returned unusable YUYV geometry %dx%d stride %d", opts.Device, d.pix.Width, d.pix.Height, d.pix.BytesPerLine)
	}

	if opts.FPS > 0 {
		var parm v4l2StreamParm
		parm.Type = v4l2BufTypeVideoCapture
		parm.Capture.TimePerFrame.Numerator, parm.Capture.TimePerFrame.Denominator = 1, uint32(opts.FPS)
		// Best effort: not every driver supports frame interval selection.
		_ = v4l2Ioctl(d.fd, vidiocSParm, unsafe.Pointer(&parm))
	}

	req := v4l2RequestBuffers{Count: v4l2BufferCount, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	if err := v4l2Ioctl(d.fd, vidiocReqBufs, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("request buffers: %w", err)
	}
	if req.Count == 0 {
		return errors.New("driver granted no capture buffers")
	}
	for i := uint32(0); i < req.Count; i++ {
		buf := v4l2Buffer{Index: i, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
		if err := v4l2Ioctl(d.fd, vidiocQueryBuf, unsafe.Pointer(&buf)); err != nil {
			return fmt.Errorf("query buffer %d: %w", i, err)
		}
		mem, err := unix.Mmap(d.fd, int64(buf.offset()), int(buf.Length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap buffer %d: %w", i, err)
		}
		d.buffers = append(d.buffers, mem)
		if err := v4l2Ioctl(d.fd, vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
			return fmt.Errorf("queue buffer %d: %w", i, err)
		}
	}
	bufType := int32(v4l2BufTypeVideoCapture)
	if err := v4l2Ioctl(d.fd, vidiocStreamOn, unsafe.Pointer(&bufType)); err != nil {
		return fmt.Errorf("stream on: %w", err)
	}
	d.streaming = true
	return nil
}

func (d *v4l2Device) Close() {
	if d.streaming {
		bufType := int32(v4l2BufTypeVideoCapture)
		_ = v4l2Ioctl(d.fd, vidiocStreamOff, unsafe.Pointer(&bufType))
		d.streaming = false
	}
	for _, b := range d.buffers {
		_ = unix.Munmap(b)
	}
	d.buffers = nil
	if d.fd >= 0 {
		_ = unix.Close(d.fd)
		d.fd = -1
	}
}

// waitFrame polls for a filled buffer, returning ok=false on poll timeout.
// A successful dequeue must be followed by requeue(buf).
func (d *v4l2Device) waitFrame() (buf v4l2Buffer, ok bool, err error) {
	fds := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, v4l2PollTimeoutMs)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return buf, false, nil
		}
		return buf, false, fmt.Errorf("poll: %w", err)
	}
	if n == 0 {
		return buf, false, nil
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 && fds[0].Revents&unix.POLLIN == 0 {
		return buf, false, errors.New("device disconnected")
	}
	buf = v4l2Buffer{Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	if err := v4l2Ioctl(d.fd, vidiocDQBuf, unsafe.Pointer(&buf)); err != nil {
		if errors.Is(err, unix.EAGAIN) {
			return buf, false, nil
		}
		return buf, false, fmt.Errorf("dequeue buffer: %w", err)
	}
	if int(buf.Index) >= len(d.buffers) {
		return buf, false, fmt.Errorf("driver returned unknown buffer index %d", buf.Index)
	}
	return buf, true, nil
}

func (d *v4l2Device) requeue(buf v4l2Buffer) error {
	if err := v4l2Ioctl(d.fd, vidiocQBuf, unsafe.Pointer(&buf)); err != nil {
		return fmt.Errorf("requeue buffer: %w", err)
	}
	return nil
}

func (c *Connector) connectV4L2Stream(ctx context.Context) error {
	opts, err := parseV4L2URL(c.cfg.URL)
	if err != nil {
		return err
	}
	dev, err := openV4L2Device(opts)
	if err != nil {
		return err
	}
	defer func() {
		dev.Close()
		c.mu.Lock()
		c.connected = false
		c.latestFrame = nil
		c.latestDecoded = nil
		c.mu.Unlock()
	}()

	pix := dev.pix
	c.setStatus(true, "")
	c.logger.Info("V4L2 camera streaming", "device", opts.Device, "card", dev.card,
		"format", fourccString(pix.PixelFormat), "width", pix.Width, "height", pix.Height)

	lastFrameAt := time.Now()
	var lastRawStored time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopCh:
			return context.Canceled
		default:
		}

		buf, ok, err := dev.waitFrame()
		if err != nil {
			return fmt.Errorf("V4L2 capture on %s failed: %w", opts.Device, err)
		}
		if !ok {
			if time.Since(lastFrameAt) > v4l2FrameTimeout {
				return fmt.Errorf("V4L2 device %s delivered no frame for %s", opts.Device, v4l2FrameTimeout)
			}
			continue
		}
		lastFrameAt = time.Now()

		used := int(buf.BytesUsed)
		data := dev.buffers[buf.Index]
		if used <= 0 || used > len(data) {
			used = len(data)
		}
		if buf.Flags&v4l2BufFlagError == 0 {
			c.framesReceived.Add(1)
			switch pix.PixelFormat {
			case v4l2PixFmtYUYV:
				if time.Since(lastRawStored) >= v4l2MinRawFrameInterval {
					lastRawStored = time.Now()
					c.setLatestDecoded(&yuyvFrame{
						width:  int(pix.Width),
						height: int(pix.Height),
						stride: int(pix.BytesPerLine),
						data:   append([]byte(nil), data[:used]...),
					})
				}
			default:
				if frame, err := normalizeMJPEGFrame(data[:used]); err != nil {
					c.logger.Debug("skipping invalid V4L2 MJPEG frame", "err", err)
				} else {
					c.setLatestFrame(frame)
				}
			}
		}
		if err := dev.requeue(buf); err != nil {
			return fmt.Errorf("V4L2 capture on %s failed: %w", opts.Device, err)
		}
	}
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
