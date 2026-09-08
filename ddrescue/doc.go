// Package ddrescue reads a disk image alongside the map file GNU ddrescue
// writes while rescuing it, so that a read touching a range ddrescue never
// recovered fails instead of returning whatever bytes happen to be in the
// image there.
//
// A map file lists byte ranges and how far ddrescue got with each: finished,
// non-tried, or failed at one of three stages. Only a finished range holds real
// data; under every other status the image's bytes are not data from the
// medium. Reader enforces that on every read, and MapBuilder writes a map in
// the same format for images this toolkit rips itself.
//
// Format reference:
//
//	https://www.gnu.org/software/ddrescue/manual/ddrescue_manual.html#Mapfile-structure
package ddrescue
