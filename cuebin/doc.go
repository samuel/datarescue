// Package cuebin works with CD images stored as a CUE sheet beside a BIN file:
// parsing the sheet, verifying the BIN's structure, extracting tracks, and
// writing both while ripping.
//
// Verification checks each sector's sync pattern, its MSF position, and — for
// MODE1 sectors — the EDC checksum, cross-referenced against a ddrescue map
// when one exists. EDC and the map disagreeing means two opposite things, so
// they are counted apart: a sector the map calls recovered but whose EDC fails
// is corruption the map missed, while the reverse usually means a stale map.
//
// Format references:
//
//	https://en.wikipedia.org/wiki/Cue_sheet_(computing)
//	https://www.gnu.org/software/ccd2cue/manual/html_node/CUE-sheet-format.html
//	https://ecma-international.org/wp-content/uploads/ECMA-130_2nd_edition_june_1996.pdf
//	https://en.wikipedia.org/wiki/CD-ROM#Sector_structure
package cuebin
