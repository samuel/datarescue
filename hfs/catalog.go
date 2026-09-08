package hfs

const (
	CatalogRecordFolder       int16 = 1
	CatalogRecordFile         int16 = 2
	CatalogRecordFolderThread int16 = 3
	CatalogRecordFileThread   int16 = 4
)

// CatalogKey is the key for every record in the catalog B-tree.
// Sorted first by ParentID, then by name (Mac Roman, case-insensitive).
type CatalogKey struct {
	KeyLength  uint8
	Reserved   uint8
	ParentID   uint32
	NameLength uint8
	Name       [31]byte
}

type CatalogFolder struct {
	RecordType    int16
	Flags         uint16
	Valence       uint16 // number of files and directories in this folder
	CNID          uint32
	CreateDate    uint32
	ModifyDate    uint32
	BackupDate    uint32
	FinderInfo    [16]byte
	FinderInfoExt [16]byte
	Reserved      [4]uint32
}

// CatalogFile is the on-disk CdrFilRec, 102 bytes. FileType and FinderInfo
// together make up the 16-byte filUsrWds (FInfo); FileType is its fdType.
type CatalogFile struct {
	RecordType          int16
	Flags               uint16
	FileType            uint32
	FinderInfo          [12]byte
	CNID                uint32
	DataForkStartBlock  uint16 // deprecated; use DataExtents
	DataForkLogicalSize uint32
	DataForkPhysSize    uint32
	RsrcForkStartBlock  uint16 // deprecated
	RsrcForkLogicalSize uint32
	RsrcForkPhysSize    uint32
	CreateDate          uint32
	ModifyDate          uint32
	BackupDate          uint32
	FinderInfoExt       [16]byte
	ClumpSize           uint16
	DataExtents         ExtentsRecord
	RsrcExtents         ExtentsRecord
	Reserved            uint32
}

// CatalogThread is the on-disk CdrThdRec, 46 bytes: it maps a CNID back to its
// parent and name, so a folder whose own record is damaged can still be placed.
//
// The reserved run is two 32-bit longs, not two 16-bit words. With 16-bit words
// the struct is four bytes short: ParentID is read from the reserved tail and
// NameLength from the high byte of the real parent ID, so every thread decodes
// as {0, ""} and cannot place anything. binary.Read decodes positionally and
// reports nothing wrong.
type CatalogThread struct {
	RecordType int16
	Reserved   [2]uint32
	ParentID   uint32
	NameLength uint8
	Name       [31]byte
}
