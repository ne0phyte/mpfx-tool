#!/usr/bin/env sh
set -x

FIRMWARE_ZIP_URL='https://cdn.inmusicbrands.com/Numark/MixtrackPlatinumFX/Mixtrack%20Platinum%20FX%20Firmware%201.10%20Win.zip'
TMP_DIR="$PWD/firmware-download"

echo "Downloading firmware update zip"
mkdir -p "$TMP_DIR" || exit
wget "$FIRMWARE_ZIP_URL" -O "$TMP_DIR/update.zip" || exit

echo "Extracting firmware update zip"
7z x -aoa "$TMP_DIR/update.zip" -o"$TMP_DIR" || exit

echo "Extracting MSI64 installer"
7z e -aoa "$TMP_DIR/*.exe" -o"$TMP_DIR" ".rsrc/MSI/MSI64" -r || exit

echo "Extracting FirmwareFile"
7z e -aoa "$TMP_DIR/MSI64" -o. FirmwareFile || exit

echo "Renaming to FirmwareFile_1.10.bin"
mv FirmwareFile "FirmwareFile_1.10.bin"

echo "Cleaning up $TMP_DIR"
rm "$TMP_DIR"/*
rmdir "$TMP_DIR"
