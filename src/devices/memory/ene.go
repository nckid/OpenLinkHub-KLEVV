package memory

// Package: Memory
// Author: Nikola Jurkovic
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/config"
	"OpenLinkHub/src/dashboard"
	"OpenLinkHub/src/logger"
	"fmt"
	"os"
	"sync"
)

// ENE DRAM controller registers. Everything above 0x8000 is reached indirectly
// through the pointer register at 0x00.
const (
	eneCommandPointer   = 0x00
	eneCommandDataByte  = 0x01
	eneCommandDataBlock = 0x03
	eneCommandReadByte  = 0x81

	eneRegisterDirect = 0x8020
	eneRegisterMode   = 0x8021
	eneRegisterApply  = 0x80A0
	eneRegisterColour = 0x8100

	eneFingerprintBase = 0xA0
	eneFingerprintLen  = 16

	eneLedsPerModule = 8
	eneFrameSize     = eneLedsPerModule * 3

	eneModeRainbow = 0x05
)

var eneAddresses = []uint8{0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77}

// eneSwapWord reverses a register's bytes. SMBus word data is transmitted low
// byte first, while the controller expects the register's high byte first, so
// swapping before the write puts them on the wire in the right order.
func eneSwapWord(register uint16) uint16 {
	return (register << 8 & 0xFF00) | register>>8
}

type eneController struct {
	file      *os.File
	address   uint8
	directSet bool
	mutex     sync.Mutex
}

func (c *eneController) setPointer(register uint16) error {
	return eneWriteWordData(c.file, c.address, eneCommandPointer, eneSwapWord(register))
}

func (c *eneController) readRegister(register uint16) (uint8, error) {
	if err := c.setPointer(register); err != nil {
		return 0, err
	}
	return eneReadByteData(c.file, c.address, eneCommandReadByte)
}

func (c *eneController) writeByte(register uint16, value uint8) error {
	if err := c.setPointer(register); err != nil {
		return err
	}
	return eneWriteByteData(c.file, c.address, eneCommandDataByte, value)
}

func (c *eneController) writeBlock(register uint16, data []byte) error {
	if err := c.setPointer(register); err != nil {
		return err
	}
	return eneWriteBlockData(c.file, c.address, eneCommandDataBlock, data)
}

// SetDirect hands the module to software control. From then on all eight LEDs
// are driven from 0x8100 onwards, and any LED whose registers are not written
// goes dark rather than holding its previous colour.
func (c *eneController) SetDirect() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.setDirect()
}

func (c *eneController) setDirect() error {
	if err := c.writeByte(eneRegisterDirect, 0x01); err != nil {
		return err
	}
	if err := c.writeByte(eneRegisterApply, 0x01); err != nil {
		return err
	}
	c.directSet = true
	return nil
}

// SetMode selects a built-in effect, e.g. mode 5 for the factory rainbow.
func (c *eneController) SetMode(mode uint8) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if err := c.writeByte(eneRegisterMode, mode); err != nil {
		return err
	}
	if err := c.writeByte(eneRegisterApply, 0x01); err != nil {
		return err
	}
	c.directSet = false
	return nil
}

// WriteFrame sends one frame of RGB triples. The controller expects R, B, G
// order, so green and blue are swapped on the way out. All 24 bytes travel in a
// single block write: the controller advances its own register pointer after
// each byte, so one pointer write covers the whole frame.
func (c *eneController) WriteFrame(buffer []byte) error {
	if len(buffer) < eneFrameSize {
		return fmt.Errorf("frame is %d bytes, expected %d", len(buffer), eneFrameSize)
	}

	frame := make([]byte, eneFrameSize)
	for i := 0; i < eneFrameSize; i += 3 {
		frame[i] = buffer[i]     // r
		frame[i+1] = buffer[i+2] // b
		frame[i+2] = buffer[i+1] // g
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if !c.directSet {
		if err := c.setDirect(); err != nil {
			return err
		}
	}

	return c.writeBlock(eneRegisterColour, frame)
}

// eneProbe identifies an ENE LED controller by its register fingerprint. Real
// ENE silicon answers the whole 0xA0-0xAF range with a non-uniform pattern,
// while the unrelated devices sharing the SMBus either refuse the pointer write
// or return a flat zero range.
func eneProbe(file *os.File, address uint8) (*eneController, error) {
	c := &eneController{file: file, address: address}

	var distinct uint8
	for offset := 0; offset < eneFingerprintLen; offset++ {
		value, err := c.readRegister(uint16(eneFingerprintBase + offset))
		if err != nil {
			return nil, err
		}
		if value != 0x00 && value != 0xFF {
			distinct++
		}
	}

	if distinct == 0 {
		return nil, fmt.Errorf("no ENE fingerprint at 0x%02X", address)
	}
	return c, nil
}

// renderUnit is one continuous strip of LEDs rendered as a single effect.
type renderUnit struct {
	keys     []int
	channels int
}

// renderUnits groups ENE modules into one strip so an effect spans the whole kit
// instead of repeating on each module. Every other device renders on its own,
// which keeps the Corsair path unchanged.
func (d *Device) renderUnits(keys []int) []renderUnit {
	var units []renderUnit
	var ene []int

	for _, k := range keys {
		if _, ok := d.eneControllers[d.Devices[k].Address]; ok {
			ene = append(ene, k)
			continue
		}
		units = append(units, renderUnit{keys: []int{k}, channels: int(d.Devices[k].LedChannels)})
	}

	if len(ene) > 1 && d.canCombineEne(ene) {
		total := 0
		for _, k := range ene {
			total += int(d.Devices[k].LedChannels)
		}
		return append(units, renderUnit{keys: ene, channels: total})
	}

	for _, k := range ene {
		units = append(units, renderUnit{keys: []int{k}, channels: int(d.Devices[k].LedChannels)})
	}
	return units
}

// canCombineEne reports whether the given ENE channels may share one render.
// They must run the same profile and none may carry an enabled per-channel
// override; per-LED colour data is inherently per channel and is left alone.
func (d *Device) canCombineEne(keys []int) bool {
	profile := d.Devices[keys[0]].RGB
	if profile == "led" {
		return false
	}

	for _, k := range keys {
		if d.Devices[k].RGB != profile {
			return false
		}
		if d.DeviceProfile != nil {
			if override := d.getRgbOverride(k, 0); override != nil && override.Enabled {
				return false
			}
		}
	}
	return true
}

// getEneDevices discovers ENE DRAM controllers and registers them as channels.
// Discovery is independent of the Corsair path: ENE modules carry no DIMM vendor
// string in the format that path requires, and their LED count is fixed by the
// controller rather than looked up in metadata.
func (d *Device) getEneDevices() int {
	baseDevice := 51
	found := 0

	for _, address := range eneAddresses {
		controller, err := eneProbe(d.dev.File, address)
		if err != nil {
			if d.Debug {
				logger.Log(logger.Fields{"address": address, "error": err}).Info("ENE probe rejected address")
			}
			continue
		}

		if err = controller.SetDirect(); err != nil {
			logger.Log(logger.Fields{"address": address, "error": err}).Error("Unable to set ENE module to direct mode")
			continue
		}

		channelId := len(d.Devices)
		label := "Set Label"
		rgbProfile := "static"

		if d.DeviceProfile != nil {
			if lb, ok := d.DeviceProfile.Labels[channelId]; ok && len(lb) > 0 {
				label = lb
			}
			if rp, ok := d.DeviceProfile.RGBProfiles[channelId]; ok {
				if d.GetRgbProfile(rp) != nil {
					rgbProfile = rp
				} else {
					logger.Log(logger.Fields{"serial": d.Serial, "profile": rp}).Warn("Tried to apply non-existing rgb profile")
				}
			}
		}

		device := &Devices{
			ChannelId:   channelId,
			DeviceId:    channelId,
			Address:     address,
			Sku:         "ENE DRAM",
			MemoryType:  d.RuntimeMemoryType,
			LedChannels: eneLedsPerModule,
			Name:        "ENE DRAM",
			Label:       label,
			RGB:         rgbProfile,
		}

		if len(d.SkuLine) < 1 {
			d.SkuLine = device.Name
		}

		// The LEDs sit on the ENE controller, but the module temperature comes
		// from the SPD hub on the same bus.
		if config.GetConfig().RamTempViaHwmon {
			if hwmonFile := d.getHwMonTemperatureFile(baseDevice+found*2, "spd5118"); len(hwmonFile) > 0 {
				device.HwmonPath = hwmonFile
				if hwmonTemp, tempErr := d.getTemperature(hwmonFile); tempErr == nil {
					device.Temperature = hwmonTemp
					device.TemperatureString = dashboard.GetDashboard().TemperatureToString(hwmonTemp)
					device.HasTemps = true
				}
			}
		}

		d.Devices[channelId] = device
		d.eneControllers[address] = controller
		d.LEDChannels += eneLedsPerModule
		found++

		logger.Log(logger.Fields{
			"address":   address,
			"channelId": channelId,
			"leds":      eneLedsPerModule,
		}).Info("Found ENE DRAM controller")
	}

	return found
}
