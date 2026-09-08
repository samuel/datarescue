// Package img86f reads floppy disk images in the 86Box 86F format.
//
// An 86F image stores the raw flux-level surface of each track rather than
// decoded sectors, so reading one means MFM-decoding the track and locating its
// address and data marks. Track lengths come from the image's own offset table,
// which is untrusted: offsets are range-checked against the file, and a track
// whose successor does not follow it falls back to the nominal length implied
// by the disk flags.
//
// NewLinearReader presents the decoded sectors as the flat byte stream a
// filesystem parser expects, given the geometry — an 86F image does not record
// sectors-per-track, heads or bytes-per-sector, so either DetectGeometry infers
// them or the caller supplies them.
//
// Format reference:
//
//	https://86box.readthedocs.io/en/latest/dev/formats/86f.html
package img86f
