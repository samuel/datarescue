// Package iso reads and writes ISO 9660 disc images.
//
// The reader handles both cooked images (2048-byte sectors) and raw ones
// (2352), and applies the Joliet and Rock Ridge extensions when a disc carries
// them, falling back to 8.3 names when it does not. If a supplementary volume
// descriptor cannot be read, Joliet names are lost, so this is recorded in
// Issues rather than silently changing which names come back.
//
// The writer produces an image plus a ddrescue map recording which of its
// sectors are real and which are zero fill, so a partial rip can be
// interpreted.
//
// Format references:
//
//	https://ecma-international.org/wp-content/uploads/ECMA-119_5th_edition_december_2024.pdf
//	https://wiki.osdev.org/ISO_9660
//	http://www.pismotec.com/cfs/jolspec.html
//	https://web.archive.org/web/20170404043745/http://www.ymi.com/ymi/sites/default/files/pdf/Rockridge.pdf
package iso
