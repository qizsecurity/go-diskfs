package crc

// CRC16 is the Linux kernel's crc16() (lib/crc16.c): CRC-16/ARC, polynomial 0x8005 processed least significant bit first
// (0xA001 reflected), no final inversion. The caller gives the starting value, as the kernel does (ext4 gdt_csum starts at ~0).
func CRC16(crc uint16, bs []byte) uint16 {
	for _, b := range bs {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
