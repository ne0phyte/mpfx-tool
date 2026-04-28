# mpfx-tool

Firmware patcher and flasher for the Numark Mixtrack Platinum FX DJ controller.

Allows patching out the "Fader Cuts" pad mode. No more crossfader movements, just MIDI note on/off messages so you can freely remap all performance pads.

---

## ⚠️ DISCLAIMER

**Use at your own risk. I am not responsible for bricked devices, damaged hardware, voided warranties, or any other damage or loss resulting from the use of this tool. Flashing unofficial or modified firmware may permanently damage your device. You have been warned.**

---

## What it does

`mpfx-tool patch` applies an unofficial firmware patch that removes the fader cuts crossfader automation from pad mode. Pad notes still fire normally.

The patch is only valid for **firmware v1.10**. Any other version will be rejected.

---

## Requirements

- Linux
- Go 1.22+ (`golang`)
- `7zip`

*Arch*: `pacman -S golang 7zip`

*Debian/Ubuntu*: `apt install golang 7zip`

*Fedora*: `dnf install golang 7zip`

---

## Usage

### 1. Extract the firmware file

Run the `get_firmware_file.sh` script. It downloads the 1.10 Update and extracts the firmware binary.

```sh
./get_firmware_file.sh
```

### 2. Build the tool

```sh
go build .
```

### 3. Patch the firmware

```sh
./mpfx-tool patch FirmwareFile_1.10.bin
```

This verifies the file against the known v1.10 MD5, applies the patch, and writes `FirmwareFile_1.10_patched.bin`. The original file is not modified.

### 4. Flash the patched firmware

Put the controller into flash mode first:

1. Hold **SHIFT** on the controller
2. Connect USB while holding SHIFT
3. Release SHIFT after ~2 seconds

Then flash:

```sh
./mpfx-tool flash FirmwareFile_1.10_patched.bin
```

The tool will auto-detect the device. If you get a permission error, run as root or add a udev rule:

```sh
echo 'KERNEL=="hidraw*", ATTRS{idVendor}=="15e4", ATTRS{idProduct}=="0055", TAG+="uaccess"' \
  | sudo tee /etc/udev/rules.d/99-numark-mixtrack.rules
sudo udevadm control --reload && sudo udevadm trigger
```
