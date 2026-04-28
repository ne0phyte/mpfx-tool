// mpfx-tool: Firmware patcher/flasher for Numark Mixtrack Platinum FX (STM32)
//
// Protocol derived from USB capture
//
// Wire format - 32-byte HID interrupt packets:
//
//   Host -> Device (EP 0x01 OUT):
//     [0]     = 0x55        magic
//     [1]     = 0x4F 'O'    direction
//     [2]     = 0x01        report ID
//     [3]     = CMD         command byte
//     [4]     = block_hi    high byte of 256-byte block number
//     [5]     = block_lo    low byte  of 256-byte block number
//     [6]     = sub_off     sub-offset within block (0x00, 0x10 … 0xF0)
//     [7]     = 0x00        always zero
//     [8]     = seq         rolling 0-3 counter
//     [9..24] = data        16 bytes of firmware
//     [25..28]= 0x00        padding
//     [29]    = 0x00        checksum high byte
//     [30..31]= csum BE     uint16 sum of bytes [9..24]
//
//   Device -> Host (EP 0x81 IN):
//     Byte[1] 0x4F -> 0x49, byte[3] CMD→CMD|0x30. Everything else echoed verbatim.
//
// Commands:
//   0xC4  Reset  - sent first; device ACKs with byte[4]=0x1A
//   0xC1  Hello  - triggers flash erase (~2s); wait 3s before sending
//   0xC2  Data   - 16 bytes/pkt, 16384 packets for 256 KB, RTT ~3ms
//   0xC4  Reset  - sent again after last data ACK
//   0xC3  Final  - commits flash; device reboots
//
// Device: VID 0x15E4  PID 0x0055  (Numark Mixtrack Platinum FX)

package main

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	TargetVID = 0x15E4
	TargetPID = 0x0055

	PacketSize = 32

	CmdReset    = 0xC4
	CmdHello    = 0xC1
	CmdData     = 0xC2
	CmdFinalize = 0xC3

	MagicByte = 0x55
	DirOut    = 0x4F
	DirIn     = 0x49
	ReportID  = 0x01

	FirmwareChunk   = 16    // bytes of firmware per packet
	SubOffsetStride = 0x10  // sub-offset step per packet
	SubOffsetWrap   = 0x100 // 16 packets per 256-byte block

	NormalTimeout = 500 * time.Millisecond
	HelloTimeout  = 10 * time.Second // C1 seems to trigger full flash erase
	FinalTimeout  = 10 * time.Second
	PostC4Delay   = 3 * time.Second // observed in capture: app waits 3s after C4 before C1
)

// --- hidraw ioctl -------------------------------------------------------------

type hidrawDevInfo struct {
	Bustype uint32
	Vendor  int16
	Product int16
}

const hidiocgrawinfo = 0x80084803

func ioctlHidrawGetInfo(fd uintptr) (hidrawDevInfo, error) {
	var info hidrawDevInfo
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, hidiocgrawinfo,
		uintptr(unsafe.Pointer(&info)))
	if errno != 0 {
		return info, errno
	}
	return info, nil
}

func findDevice() (string, error) {
	matches, _ := filepath.Glob("/dev/hidraw*")
	for _, path := range matches {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		info, err := ioctlHidrawGetInfo(f.Fd())
		f.Close()
		if err != nil {
			continue
		}
		if uint16(info.Vendor) == TargetVID && uint16(info.Product) == TargetPID {
			return path, nil
		}
	}
	return "", fmt.Errorf("device VID=0x%04X PID=0x%04X not found (controller in flash mode?)",
		TargetVID, TargetPID)
}

// --- Packet construction ------------------------------------------------------

// buildPacket constructs a 32-byte HID output packet.
//
//	blockNum  = i / 16  (which 256-byte block we are in)
//	subOffset = (i % 16) * 0x10  (byte position within that block)
//	seq       = i % 4
//	data      = firmware[i*16 : (i+1)*16]
func buildPacket(cmd byte, blockNum uint16, subOffset byte, seq byte, data []byte) [PacketSize]byte {
	var pkt [PacketSize]byte
	pkt[0] = MagicByte
	pkt[1] = DirOut
	pkt[2] = ReportID
	pkt[3] = cmd
	pkt[4] = byte(blockNum >> 8)   // block high byte
	pkt[5] = byte(blockNum & 0xFF) // block low byte
	pkt[6] = subOffset             // sub-offset within block
	pkt[7] = 0x00
	pkt[8] = seq
	if len(data) == FirmwareChunk {
		copy(pkt[9:25], data)
		var csum uint16
		for _, b := range data {
			csum += uint16(b)
		}
		binary.BigEndian.PutUint16(pkt[30:32], csum)
	}
	return pkt
}

// --- I/O ---------------------------------------------------------------------

func sendAndACK(f *os.File, pkt [PacketSize]byte, timeout time.Duration, verbose bool) error {
	if verbose {
		fmt.Printf("  --> %s\n", hex.EncodeToString(pkt[:]))
	}

	f.SetDeadline(time.Now().Add(timeout))

	if _, err := f.Write(pkt[:]); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	var resp [PacketSize]byte
	n := 0
	for n < PacketSize {
		r, err := f.Read(resp[n:])
		n += r
		if err != nil {
			if n == PacketSize {
				break
			}
			return fmt.Errorf("read ACK (%d/%d bytes): %w", n, PacketSize, err)
		}
	}

	if verbose {
		fmt.Printf("  <-- %s\n", hex.EncodeToString(resp[:]))
	}

	expectedAck := pkt[3] | 0x30
	if resp[0] != MagicByte || resp[1] != DirIn || resp[2] != ReportID || resp[3] != expectedAck {
		return fmt.Errorf("bad ACK: got %s, want %02x %02x %02x %02x ...",
			hex.EncodeToString(resp[:4]), MagicByte, DirIn, ReportID, expectedAck)
	}
	// Verify echo of bytes [4..31]
	if pkt[3] == CmdData {
		for i := 4; i < PacketSize; i++ {
			if resp[i] != pkt[i] {
				return fmt.Errorf("ACK mismatch at byte %d: sent %02x got %02x", i, pkt[i], resp[i])
			}
		}
	}
	return nil
}

// ---- Flash sequence -----------------------------------------------------------

func flash(devPath string, firmware []byte, verbose bool) error {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()

	total := len(firmware) / FirmwareChunk
	fmt.Printf("Device:   %s\n", devPath)
	fmt.Printf("Firmware: %d bytes → %d packets (%d blocks)\n\n",
		len(firmware), total, total/16)

	// 1. C4 Reset
	fmt.Print("Step 1/6  Reset (C4)...              ")
	if err := sendAndACK(f, buildPacket(CmdReset, 0, 0, 0, nil), NormalTimeout, verbose); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	fmt.Println("OK")

	// 2. Wait for device to prepare
	fmt.Printf("Step 2/6  Waiting %s for device...   ", PostC4Delay)
	time.Sleep(PostC4Delay)
	fmt.Println("OK")

	// 3. C1 Hello (triggers flash erase)
	fmt.Print("Step 3/6  Hello (C1)...              ")
	if err := sendAndACK(f, buildPacket(CmdHello, 0, 0, 0, nil), HelloTimeout, verbose); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	fmt.Println("OK")

	// 4. C2 Data
	fmt.Printf("Step 4/6  Flashing %d packets...    ", total)
	if verbose {
		fmt.Println()
	}

	start := time.Now()
	for i := range total {
		blockNum := uint16(i / 16)
		subOffset := byte((i % 16) * SubOffsetStride)
		seq := byte(i % 4)
		chunk := firmware[i*FirmwareChunk : (i+1)*FirmwareChunk]

		pkt := buildPacket(CmdData, blockNum, subOffset, seq, chunk)

		if verbose {
			fmt.Printf("  [%5d/%d] block=0x%04x sub=0x%02x seq=%d data=%s\n",
				i+1, total, blockNum, subOffset, seq, hex.EncodeToString(chunk))
		}

		if err := sendAndACK(f, pkt, NormalTimeout, verbose); err != nil {
			return fmt.Errorf("data packet %d (block=0x%04x sub=0x%02x fw_offset=0x%05x): %w",
				i, blockNum, subOffset, i*FirmwareChunk, err)
		}

		if !verbose {
			pct := (i + 1) * 100 / total
			filled := pct / 5
			bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
			eta := time.Duration(0)
			if i > 0 {
				eta = time.Since(start) * time.Duration(total-i-1) / time.Duration(i+1)
			}
			fmt.Printf("\r  [%s] %3d%%  %d/%d  ETA %s   ",
				bar, pct, i+1, total, eta.Round(time.Second))
		}
	}
	elapsed := time.Since(start)
	if !verbose {
		fmt.Printf("\r  [████████████████████] 100%%  %d/%d  %s          \n",
			total, total, elapsed.Round(time.Millisecond))
	}
	fmt.Println("OK")

	// 5. C4 Reset (second, before finalize)
	fmt.Print("Step 5/6  Reset (C4)...              ")
	if err := sendAndACK(f, buildPacket(CmdReset, 0, 0, 0, nil), NormalTimeout, verbose); err != nil {
		return fmt.Errorf("second reset: %w", err)
	}
	fmt.Println("OK")

	// 6. C3 Finalize
	fmt.Print("Step 6/6  Finalize (C3)...           ")
	if err := sendAndACK(f, buildPacket(CmdFinalize, 0, 0, 0, nil), FinalTimeout, verbose); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	fmt.Println("OK")

	fmt.Printf("\nFlash complete in %s. Device rebooting.\n", elapsed.Round(time.Millisecond))
	return nil
}

// --- main ---------------------------------------------------------------------

const (
	FirmwareSize    = 0x40000
	FirmwareV110MD5 = "d9dae8d47af7857b8c6c3f260788ee48" // FirmwareFile v1.10 (unpatched)
)

// patchSite describes a single byte to verify and replace.
type patchSite struct {
	offset   int
	old, new byte
}

// firmwarePatch is the fader-cuts removal patch:
// Changes BCC → B at 0x18e5c in function 0x08018e46, causing the fader cuts
// code path to always exit without sending any crossfader CC messages.
var firmwarePatch = []patchSite{
	{0x18e5c, 0x01, 0x8f},
	{0x18e5d, 0xd3, 0xe0},
}

func cmdPatch(args []string) {
	fs := flag.NewFlagSet("patch", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mpfx-tool patch <firmware.bin>\n\n")
		fmt.Fprintf(os.Stderr, "Applies the fader-cuts removal patch and writes <firmware>_patched.bin.\n")
		fmt.Fprintf(os.Stderr, "The original file is not modified.\n")
	}
	fs.Parse(args)
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	inputPath := fs.Arg(0)
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading firmware: %v\n", err)
		os.Exit(1)
	}

	data := make([]byte, len(raw))
	copy(data, raw)

	if len(data) != FirmwareSize {
		fmt.Fprintf(os.Stderr, "Error: unexpected file size %#x, expected %#x (256 KB)\n",
			len(data), FirmwareSize)
		os.Exit(1)
	}

	// Verify this is the known unpatched v1.10 firmware
	sum := md5.Sum(data)
	got := hex.EncodeToString(sum[:])
	if got != FirmwareV110MD5 {
		fmt.Fprintf(os.Stderr, "Error: MD5 mismatch - this does not look like unpatched v1.10 firmware.\n")
		fmt.Fprintf(os.Stderr, "  expected: %s\n", FirmwareV110MD5)
		fmt.Fprintf(os.Stderr, "  got:      %s\n", got)
		os.Exit(1)
	}
	fmt.Printf("MD5 verified: %s ✓\n", got)

	// Apply patch
	for _, p := range firmwarePatch {
		data[p.offset] = p.new
	}
	fmt.Printf("Patch applied at 0x%05x: BCC → B  (fader cuts disabled)\n", firmwarePatch[0].offset)

	// Write output next to the input file
	ext := filepath.Ext(inputPath)
	base := strings.TrimSuffix(inputPath, ext)
	outputPath := base + "_patched" + ext
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Written: %s\n", outputPath)
}

func cmdFlash(args []string) {
	fs := flag.NewFlagSet("flash", flag.ExitOnError)
	devFlag := fs.String("device", "", "hidraw path, e.g. /dev/hidraw0 (auto-detected if omitted)")
	verboseFlag := fs.Bool("verbose", false, "print every packet and ACK")
	dryRunFlag := fs.Bool("dry-run", false, "validate firmware file without touching the device")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mpfx-tool flash [flags] <firmware.bin>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}

	firmware, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading firmware: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Numark Mixtrack Platinum FX - Firmware Patcher/Flasher\n")
	fmt.Printf("================================================\n")
	fmt.Printf("Firmware: %s\n", fs.Arg(0))
	fmt.Printf("  Size:   %d bytes (%d KB, %d packets, %d blocks)\n",
		len(firmware), len(firmware)/1024,
		len(firmware)/FirmwareChunk, len(firmware)/256)
	fmt.Printf("  First:  %s\n", hex.EncodeToString(firmware[:min(16, len(firmware))]))
	fmt.Printf("  Last:   %s\n\n", hex.EncodeToString(firmware[max(0, len(firmware)-16):]))

	if len(firmware) == 0 || len(firmware)%FirmwareChunk != 0 {
		fmt.Fprintf(os.Stderr, "Error: firmware must be non-empty and a multiple of %d bytes\n",
			FirmwareChunk)
		os.Exit(1)
	}
	if *dryRunFlag {
		fmt.Println("Dry run - firmware looks valid.")
		return
	}

	devPath := *devFlag
	if devPath == "" {
		fmt.Printf("Auto-detecting device (VID=0x%04X PID=0x%04X)...\n", TargetVID, TargetPID)
		devPath, err = findDevice()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
			fmt.Fprintln(os.Stderr, "To enter flash mode:")
			fmt.Fprintln(os.Stderr, "  1. Hold SHIFT on the controller")
			fmt.Fprintln(os.Stderr, "  2. Connect USB while holding SHIFT")
			fmt.Fprintln(os.Stderr, "  3. Release SHIFT after ~2 seconds")
			fmt.Fprintln(os.Stderr, "\nTo avoid needing sudo:")
			fmt.Fprintln(os.Stderr, `  echo 'KERNEL=="hidraw*", ATTRS{idVendor}=="15e4", ATTRS{idProduct}=="0055", TAG+="uaccess"' \`)
			fmt.Fprintln(os.Stderr, `    | sudo tee /etc/udev/rules.d/99-numark-mixtrack.rules`)
			fmt.Fprintln(os.Stderr, "  sudo udevadm control --reload && sudo udevadm trigger")
			os.Exit(1)
		}
		fmt.Printf("Found: %s\n\n", devPath)
	}

	if err := flash(devPath, firmware, *verboseFlag); err != nil {
		fmt.Fprintf(os.Stderr, "\nFlash failed: %v\n", err)
		os.Exit(1)
	}
}

func main() {
	usage := func() {
		fmt.Fprintf(os.Stderr, `Numark Mixtrack Platinum FX - firmware tool

Usage:
  mpfx-tool patch  <firmware.bin>            patch out fader cuts, write *_patched.bin
  mpfx-tool flash  [flags] <firmware.bin>    flash firmware to device
  mpfx-tool flash  --help                    flash flags

`)
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "patch":
		cmdPatch(os.Args[2:])
	case "flash":
		cmdFlash(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}
