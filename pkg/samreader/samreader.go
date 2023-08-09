package samreader

import (
	"bytes"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/lesnuages/gosecretsdump/pkg/systemreader"
	"github.com/lesnuages/gosecretsdump/pkg/winregistry"
	"golang.org/x/text/encoding/unicode"

	"github.com/lesnuages/gosecretsdump/pkg/ditreader"
)

// New Creates a new dit dumper
func New(system, sam string) (SamReader, error) {
	r := SamReader{
		noLMHash:           true,
		remoteOps:          "",
		samLoc:             sam,
		systemHiveLocation: system,
		//db:                 esent.Esedb{}.Init(ntds),
		userData: make(chan ditreader.DumpedHash, 500),
	}
	var err error
	r.registry, err = winregistry.InitOffline(sam)
	if r.systemHiveLocation != "" {
		ls, err := systemreader.New(r.systemHiveLocation)
		if err != nil {
			return r, err
		}
		r.bootKey, err = ls.BootKey()
		if err != nil {
			return r, err
		}
		r.noLMHash = ls.HasNoLMHashPolicy()
	} else {
		return r, fmt.Errorf("System hive empty")
	}
	return r, err
}

func NewLive() (SamReader, error) {
	r := SamReader{
		noLMHash:  true,
		remoteOps: "",
		userData:  make(chan ditreader.DumpedHash, 500),
	}
	var err error
	r.registry, err = winregistry.InitLive("SAM")

	ls, err := systemreader.NewLive()
	if err != nil {
		return r, err
	}
	r.bootKey, err = ls.BootKey()
	if err != nil {
		return r, err
	}
	r.noLMHash = ls.HasNoLMHashPolicy()
	return r, err
}

type SamReader struct {
	samFile            *os.File
	bootKey            []byte
	noLMHash           bool
	remoteOps          string
	samLoc             string
	systemHiveLocation string
	registry           winregistry.WinRegIF
	userData           chan ditreader.DumpedHash
}

func (d *SamReader) dump() {
	d.Dump()
}

// GetOutChan returns a reference to the objects output channel for read only operations
func (d SamReader) GetOutChan() <-chan ditreader.DumpedHash {
	return d.userData
}

type SAMKeyData struct {
	Revision uint32 //2
	Length   uint32
	Salt     [16]byte
	Key      [16]byte
	Checksum [16]byte
	Reserved [2]uint32
}

type SAMKeyDataAES struct {
	Revision uint32 //3
	Length   uint32
	CheckLen uint32
	DataLen  uint32
	Salt     [16]byte
	Data     [32]byte
}

type Domain_Account_F struct {
	f_Details
	Data []byte
	//_                        uint32
}

type f_Details struct {
	Revision uint16
	_        uint32
	_        uint16
	CreationTime, DomainModifiedCount,
	MaxPasswordage, MinPasswordAge,
	ForceLogoff, LockoutDuration,
	LockoutObservationWindow, ModifiedCountAtLastPromotion uint64
	NextRid                  uint32
	PasswordProperties       uint32
	MinPasswordLength        uint16
	PasswordHistoryLength    uint16
	LockoutThreshold         uint16
	_                        uint16
	ServerState              uint32
	ServerRole               uint32
	UasCompatibilityRequired uint32
	_                        uint32
}

type KeyData struct {
	Key []byte
}

func NewF(b []byte) Domain_Account_F {
	r := Domain_Account_F{}
	d := bytes.NewReader(b)
	binary.Read(d, binary.LittleEndian, &r.f_Details)

	r.Data = make([]byte, d.Len())
	d.Read(r.Data)

	return r
}

func (d SamReader) SysKey() ([]byte, error) {
	_, fraw, err := d.registry.GetVal("\\SAM\\Domains\\Account\\F")
	if err != nil {
		return nil, err
	}
	f := NewF(fraw)
	if f.Revision == 3 {
		aesStruct := SAMKeyDataAES{}
		binary.Read(bytes.NewReader(f.Data), binary.LittleEndian, &aesStruct)
		iv := aesStruct.Salt[:]
		cipher := aesStruct.Data[:aesStruct.DataLen]
		b, e := ditreader.DecryptAES(d.bootKey, cipher, iv)
		if e != nil {
			return nil, e
		}
		return b[:16], nil
	} else if f.Revision == 2 {
		rc4struct := SAMKeyData{}
		binary.Read(bytes.NewReader(f.Data), binary.LittleEndian, &rc4struct)
		hashdata := append(rc4struct.Salt[:], qwertyconst...)
		hashdata = append(hashdata, d.bootKey...)
		hashdata = append(hashdata, digitconst...)
		rc4Key := md5.Sum(hashdata)
		rc4life, e := rc4.NewCipher(rc4Key[:])
		if e != nil {
			return nil, e
		}
		d := make([]byte, 32)
		rc4life.XORKeyStream(d, append(rc4struct.Key[:], rc4struct.Checksum[:]...))
		//todo do checksum
		//impacket:
		/*
		   # Verify key with checksum
		   checkSum = self.MD5( self.__hashedBootKey[:16] + DIGITS + self.__hashedBootKey[:16] + QWERTY)
		   if checkSum != self.__hashedBootKey[16:]:
		       raise Exception('hashedBootKey CheckSum failed, Syskey startup password probably in use! :(')
		*/
		return d[:16], nil
	} else {
		return nil, fmt.Errorf("not yet implemented")
	}
}

var qwertyconst = []byte("!@#$%^&*()qwertyUIOPAzxcvbnmQQQQQQQQQQQQ)(*@&%\x00")
var digitconst = []byte("0123456789012345678901234567890123456789\x00")

func (d SamReader) parseV(i uint32) (User_Account_V, error) {
	var err error
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, i)
	key := fmt.Sprintf("\\SAM\\Domains\\Account\\Users\\%s\\V", strings.ToUpper(hex.EncodeToString(b)))
	_, vraw, err := d.registry.GetVal(key)
	if err != nil {
		return User_Account_V{}, fmt.Errorf("Bad result: %s (%s) %d", err, key, i)
	}

	return newV(vraw), nil
}

type SAMEntry struct {
	Offset uint32
	Length uint32
	_      uint32
}

func (s SAMEntry) GetData(b []byte) []byte {
	return b[s.Offset : s.Offset+s.Length]
}

type User_Account_V struct {
	SAMEntries
	Data []byte
}

type SAMEntries struct {
	_,
	Username, FullName, Comment,
	UserComment,
	_,
	Homedir,
	HomedirConnect,
	ScriptPath,
	ProfilePath,
	Workstations,
	HoursAllowed,
	_,
	LMHash,
	NTLMHash,
	NTLMHistory,
	LMHistory SAMEntry
}

func (u User_Account_V) UsernameString() string {
	ud := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	b, e := ud.Bytes(u.Username.GetData(u.Data))
	if e != nil {
		return string(u.Username.GetData(u.Data))
	}
	return string(b)
}

func newV(d []byte) User_Account_V {
	r := User_Account_V{}
	rd := bytes.NewReader(d)
	binary.Read(rd, binary.LittleEndian, &r.SAMEntries)
	r.Data = make([]byte, rd.Len())
	rd.Read(r.Data)
	return r
}

func (d SamReader) GetRids() ([]uint32, error) {
	x, err := d.registry.EnumKeys("\\SAM\\Domains\\Account\\Users")
	if err != nil {
		return []uint32{}, err
	}
	r := []uint32{}
	for _, i := range x {
		b, e := hex.DecodeString(i)
		if e != nil {
			//this indicates the 'Names' key... probably could handle it a bit better I gues
			continue
		}
		r = append(r, binary.BigEndian.Uint32(b))
	}
	return r, nil
}

type SAMHashAES struct {
	SAMHashAESInfo
	Hash []byte
}

type SAMHashAESInfo struct {
	PekID      uint16
	Revision   uint16
	DataOffset uint32
	Salt       [16]byte
}

type SAMHash struct {
	PekID    uint16
	Revision uint16
	Hash     [16]byte
}

func NewSamHashAES(b []byte) SAMHashAES {
	br := bytes.NewReader(b)
	r := SAMHashAES{}
	binary.Read(br, binary.LittleEndian, &r.SAMHashAESInfo)
	r.Hash = make([]byte, br.Len())
	br.Read(r.Hash)
	return r
}

func (d SamReader) Dump() error {
	defer close(d.userData)
	boot, err := d.SysKey()
	if err != nil {
		return err
	}
	rids, _ := d.GetRids()
	for _, rid := range rids {
		v, err := d.parseV(rid)
		if err != nil {
			return err
		}
		data := v.NTLMHash.GetData(v.Data)
		if data[0] == 2 {
			//new style (AES)
			a := NewSamHashAES(data)
			if len(a.Hash) > 0 {
				raw, err := ditreader.DecryptAES(boot, a.Hash, a.Salt[:])
				if err != nil {
					return err
				}
				data = raw[:16]
			} else {
				data = []byte{}
			}
		} else {
			if v.NTLMHash.Length == 20 {
				sh := SAMHash{}
				binary.Read(bytes.NewReader(data), binary.LittleEndian, &sh)
				data = sh.Hash[:]
			} else {
				data = []byte{}
			}
		}
		ntlmplain := ditreader.EmptyNT
		if len(data) > 0 {
			ntlmplain, err = ditreader.RemoveDES(data, rid)
			if err != nil {
				return err
			}
		}
		d.userData <- ditreader.DumpedHash{
			Username: v.UsernameString(),
			LMHash:   ditreader.EmptyLM,
			NTHash:   ntlmplain,
			Rid:      rid,
		}
	}
	return nil
}
