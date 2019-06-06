package libdumpsecrets

import (
	"bytes"
	"crypto/md5"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	u "unicode"

	"golang.org/x/text/encoding/unicode"

	"github.com/c-sto/gosecretsdump/libdumpsecrets/esent"
	"github.com/c-sto/gosecretsdump/libdumpsecrets/winregistry"
)

//This is essentially a direct translation of impacket's secretsdump. I did not reverse any of the file stuff, just translated it into golang
//all credit should go to the impacket team for that

//this file (and the other impackety libs I use) will slowly be optimized once I get a feel for how everything works
//ideally making everything more parallel friendly etc

type Gosecretsdump struct {
	ntdsFile  *os.File
	bootKey   []byte
	isRemote  bool
	history   bool
	noLMHash  bool
	remoteOps string

	useVSSMethod       bool
	justNTLM           bool
	pwdLastSet         bool
	resumeSession      string
	outputFileName     string
	systemHiveLocation string
	ntdsFileLocation   string

	justUser        string
	printUserStatus bool

	perSecretCallback bool // nil
	secret            bool //nil

	resumeSessionMgr bool // nil

	db       esent.Esedb
	cursor   *esent.Cursor
	pek      [][]byte
	tmpUsers []esent.Esent_record

	settings Settings
}

//global maps are probably not the best way of doing this, but it will do for now
var accTypes = map[int32]string{
	0x30000000: "SAM_NORMAL_USER_ACCOUNT",
	0x30000001: "SAM_MACHINE_ACCOUNT",
	0x30000002: "SAM_TRUST_ACCOUNT",
}

var nToInternal = map[string]string{
	"uSNCreated":              "ATTq131091",
	"uSNChanged":              "ATTq131192",
	"name":                    "ATTm3",
	"objectGUID":              "ATTk589826",
	"objectSid":               "ATTr589970",
	"userAccountControl":      "ATTj589832",
	"primaryGroupID":          "ATTj589922",
	"accountExpires":          "ATTq589983",
	"logonCount":              "ATTj589993",
	"sAMAccountName":          "ATTm590045",
	"sAMAccountType":          "ATTj590126",
	"lastLogonTimestamp":      "ATTq589876",
	"userPrincipalName":       "ATTm590480",
	"unicodePwd":              "ATTk589914",
	"dBCSPwd":                 "ATTk589879",
	"ntPwdHistory":            "ATTk589918",
	"lmPwdHistory":            "ATTk589984",
	"pekList":                 "ATTk590689",
	"supplementalCredentials": "ATTk589949",
	"pwdLastSet":              "ATTq589920",
}

type Settings struct {
	SystemLoc   string
	NTDSLoc     string
	Status      bool
	EnabledOnly bool
	Outfile     string
	NoPrint     bool
}

func (g Gosecretsdump) Init(s Settings) Gosecretsdump {
	r := Gosecretsdump{
		isRemote:           false,
		history:            false,
		noLMHash:           true,
		remoteOps:          "",
		useVSSMethod:       false,
		justNTLM:           false,
		pwdLastSet:         false,
		resumeSession:      "",
		outputFileName:     "",
		justUser:           "",
		printUserStatus:    false,
		systemHiveLocation: s.SystemLoc,
		ntdsFileLocation:   s.NTDSLoc,
		db:                 esent.Esedb{}.Init(s.NTDSLoc),
	}
	r.settings = s
	r.cursor = r.db.OpenTable("datatable")

	return r
}

func (g *Gosecretsdump) GetPek() bool {
	pekList := []byte{}
	for {
		record, err := g.db.GetNextRow(g.cursor)
		if err != nil && err.Error() != "ignore" {
			panic(err)
		}
		if err != nil && err.Error() == "ignore" {
			break //lol fml
		}
		if v, ok := record.Column[nToInternal["pekList"]]; ok && len(v.BytVal) > 0 {
			pekList = v.BytVal
			break
		}
		if _, ok := record.Column[nToInternal["sAMAccountType"]]; ok {
			//users found?
			g.tmpUsers = append(g.tmpUsers, record)
		}
	}
	if len(pekList) > 0 { //not an empty pekkyboi
		encryptedPekList := peklist_enc{}.Init(pekList)
		if bytes.Compare(encryptedPekList.Header[:4], []byte("\x02\x00\x00\x00")) == 0 {
			//up to windows 2012 r2 something something
			md := md5.New()
			md.Write(g.bootKey)
			for i := 0; i < 1000; i++ {
				md.Write(encryptedPekList.KeyMaterial[:])
			}
			tmpKey := md.Sum([]byte{})
			rc, err := rc4.NewCipher(tmpKey)
			if err != nil {
				panic(err)
			}
			dst := make([]byte, len(encryptedPekList.EncryptedPek))
			rc.XORKeyStream(dst, encryptedPekList.EncryptedPek)
			decryptedPekList := peklist_plain{}.Init(dst)
			pekLen := 20 //len of the pek_key structure
			for i := 0; i < len(decryptedPekList.DecryptedPek)/pekLen; i++ {
				cursor := i * pekLen
				pek := pek_key{}.Init(decryptedPekList.DecryptedPek[cursor : cursor+pekLen])
				fmt.Println("PEK found and decrypted:", hex.EncodeToString(pek.Key[:]))
				g.pek = append(g.pek, pek.Key[:])
			}
		} else if bytes.Compare(encryptedPekList.Header[:4], []byte("\x03\x00\x00\x00")) == 0 {
			//something something 2016 TP4
			/*
				# Windows 2016 TP4 header starts this way
				# Encrypted PEK Key seems to be different, but actually similar to decrypting LSA Secrets.
				# using AES:
				# Key: the bootKey
				# CipherText: PEKLIST_ENC['EncryptedPek']
				# IV: PEKLIST_ENC['KeyMaterial']
			*/
			ePek := decryptAES(g.bootKey, encryptedPekList.EncryptedPek, encryptedPekList.KeyMaterial[:])
			decryptedPekList := peklist_plain{}.Init(ePek)
			g.pek = append(g.pek, decryptedPekList.DecryptedPek[4:20])
		}
	}
	return false
}

type localOps struct {
	systemLoc string
}

func (l localOps) Init(s string) localOps {
	r := localOps{systemLoc: s}
	return r
}

func (l localOps) getBootKey() []byte {
	bk := []byte{}
	tmpKey := ""
	winreg := winregistry.WinregRegistry{}.Init(l.systemLoc, false)
	//get control set
	_, bcurrentControlset, err := winreg.GetVal("\\Select\\Current")
	if err != nil {
		panic(err)
	}

	currentControlset := fmt.Sprintf("ControlSet%03d", binary.LittleEndian.Uint32(bcurrentControlset))
	for _, k := range []string{"JD", "Skew1", "GBG", "Data"} {
		ans := winreg.GetClass(fmt.Sprintf("\\%s\\Control\\Lsa\\%s", currentControlset, k))
		ud := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
		nansLen := 16
		if len(ans) < 16 {
			nansLen = len(ans)
		}
		digit := make([]byte, len(ans[:nansLen])/2)
		ud.Transform(digit, ans[:16], false)
		tmpKey = tmpKey + strings.Replace(string(digit), "\x00", "", -1)
	}
	transforms := []int{8, 5, 4, 2, 11, 9, 13, 3, 0, 6, 1, 12, 14, 10, 15, 7}
	unhexedKey, err := hex.DecodeString(tmpKey)
	if err != nil {
		panic(err)
	}
	for i := 0; i < len(unhexedKey); i++ {
		bk = append(bk, unhexedKey[transforms[i]])
	}
	fmt.Println("Target system bootkey: ", "0x"+hex.EncodeToString(bk))
	return bk
}

func (l localOps) checkNoLMHashPolicy() bool {
	winreg := winregistry.WinregRegistry{}.Init(l.systemLoc, false)
	_, bcurrentControlSet, err := winreg.GetVal("\\Select\\Current")
	if err != nil {
		fmt.Println("ERROR GETTING CONTROL SET FOR LM HASH", err)
	}
	currentControlSet := fmt.Sprintf("ControlSet%03d", binary.LittleEndian.Uint32(bcurrentControlSet))
	_, _, err = winreg.GetVal(fmt.Sprintf("\\%s\\Control\\Lsa\\NoLmHash", currentControlSet))
	if err != nil && err.Error() == winregistry.NONE {
		//yee got some LM HASHES life is gonna be GOOD
		return false
	}
	return true
}

type SAMR_USER_PROPERTIES struct { /*
			# 2.2.10.1 USER_PROPERTIES
		class USER_PROPERTIES(Structure):
		    structure = (
		        ('Reserved1','<L=0'), L = 4 bytes (uint32)
		        ('Length','<L=0'),
		        ('Reserved2','<H=0'), H = 2 bytes (uint16)
		        ('Reserved3','<H=0'),
		        ('Reserved4','96s=""'), S = 1byte (String)
		        ('PropertySignature','<H=0x50'),
		        ('PropertyCount','<H=0'),
		        ('UserProperties',':'),
		)*/
	Reserved1         uint32
	Length            uint32
	Reserved2         uint16
	Reserved3         uint16
	Reserved4         [96]byte
	PropertySignature uint16
	PropertyCount     uint16
	Properties        []SAMR_USER_PROPERTY
}

func (s SAMR_USER_PROPERTIES) New(data []byte) SAMR_USER_PROPERTIES {
	r := SAMR_USER_PROPERTIES{}
	lData := make([]byte, len(data)) //avoid mutate
	copy(lData, data)

	r.Reserved1 = binary.LittleEndian.Uint32(lData[:4])
	lData = lData[4:]
	r.Length = binary.LittleEndian.Uint32(lData[:4])
	lData = lData[4:]
	r.Reserved2 = binary.LittleEndian.Uint16(lData[:2])
	lData = lData[2:]
	r.Reserved3 = binary.LittleEndian.Uint16(lData[:2])
	lData = lData[2:]
	copy(r.Reserved4[:], lData[:96])
	lData = lData[96:]
	r.PropertySignature = binary.LittleEndian.Uint16(lData[:2])
	lData = lData[2:]
	r.PropertyCount = binary.LittleEndian.Uint16(lData[:2])
	lData = lData[2:]
	//fill properties
	for i := uint16(0); i < r.PropertyCount; i++ {
		np := SAMR_USER_PROPERTY{}
		np.NameLength = binary.LittleEndian.Uint16(lData[:2])
		lData = lData[2:]
		np.ValueLength = binary.LittleEndian.Uint16(lData[:2])
		lData = lData[2:]
		np.Reserved = binary.LittleEndian.Uint16(lData[:2])
		lData = lData[2:]
		np.PropertyName = lData[:np.NameLength]
		lData = lData[np.NameLength:]
		np.PropertyValue = lData[:np.ValueLength]
		lData = lData[np.ValueLength:]
		r.Properties = append(r.Properties, np)
	}
	return r
}

type SAMR_USER_PROPERTY struct {
	/*
			class USER_PROPERTY(Structure):
		    structure = (
		        ('NameLength','<H=0'),
		        ('ValueLength','<H=0'),
		        ('Reserved','<H=0'),
		        ('_PropertyName','_-PropertyName', "self['NameLength']"),
		        ('PropertyName',':'),
		        ('_PropertyValue','_-PropertyValue', "self['ValueLength']"),
		        ('PropertyValue',':'),
		)
	*/
	NameLength    uint16
	ValueLength   uint16
	Reserved      uint16
	PropertyName  []byte
	PropertyValue []byte
}

type SAMR_RPC_SID struct {
	Revision            uint8   //'<B'
	SubAuthorityCount   uint8   //'<B'
	IdentifierAuthority [6]byte //SAMR_RPC_SID_IDENTIFIER_AUTHORITY
	SubLen              int     //    ('SubLen','_-SubAuthority','self["SubAuthorityCount"]*4'),
	SubAuthority        []byte  //':'
}

func (s SAMR_RPC_SID) FormatCanonical() string {
	ans := fmt.Sprintf("S-%d-%d", s.Revision, s.IdentifierAuthority[5])
	for i := 0; i < int(s.SubAuthorityCount); i++ {
		ans += fmt.Sprintf("-%d", binary.BigEndian.Uint32(s.SubAuthority[i*4:i*4+4]))
	}
	return ans
}

func (s SAMR_RPC_SID) Init(data []byte) (SAMR_RPC_SID, error) {
	r := SAMR_RPC_SID{}
	if len(data) < 6 {
		return r, fmt.Errorf("Bad SAMR data: %s", string(data))
	}
	lData := make([]byte, len(data)) //avoid mutate
	copy(lData, data)

	r.Revision = lData[0]
	r.SubAuthorityCount = lData[1]
	lData = lData[2:]
	copy(r.IdentifierAuthority[:], lData[:6])
	lData = lData[6:]
	r.SubLen = int(r.SubAuthorityCount) * 4
	r.SubAuthority = make([]byte, len(lData))
	copy(r.SubAuthority, lData)
	return r, nil
}

type crypted_hashw16 struct {
	Header       [8]byte
	KeyMaterial  [16]byte
	Unknown      uint32
	EncrypedHash [32]byte
}

func (c crypted_hashw16) Init(inData []byte) crypted_hashw16 {
	r := crypted_hashw16{}
	data := make([]byte, len(inData))
	copy(data, inData)
	copy(r.Header[:], data[:8])
	data = data[8:]
	copy(r.KeyMaterial[:], data[:16])
	data = data[16:]

	r.Unknown = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	copy(r.EncrypedHash[:], data[:32])

	return r
}

type suppInfo struct {
	Username      string
	ClearPassword string
	MachinePass   bool
}

func (s suppInfo) HashString() string {
	return fmt.Sprintf("%s:CLEARTEXT:%s", s.Username, s.ClearPassword)
}

func (g *Gosecretsdump) decryptSupp(record esent.Esent_record) (suppInfo, error) {
	r := suppInfo{}
	///*
	if g.useVSSMethod {
		val := record.Column[nToInternal["supplementalCredentials"]]
		if len(val.BytVal) > 24 { //is the value above the minimum for plaintex passwords?
			username := ""
			//check if the record is something something? has a UPN?
			if record.Column[nToInternal["userPrincipalName"]].StrVal != "" {
			} else {
				username = record.Column[nToInternal["sAMAccountName"]].StrVal
			}
			//fmt.Println(val.BytVal)
			ct := crypted_hash{}.Init(val.BytVal)

			//check for windows 2016 tp4
			if bytes.Compare(ct.Header[:4], []byte{0x13, 0, 0, 0}) == 0 {
				fmt.Println("TODO: WINDOWS 2016 SUPP DATA FOR PLAINTEXT")
			} else {
				plainBytes := g.removeRC4(ct)
				props := SAMR_USER_PROPERTIES{}.New(plainBytes)
				for _, x := range props.Properties {
					//apparently we should care about kerberos-newer-keys, but I don't really want to at the moment
					s, e := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().String(string(x.PropertyName))
					if e != nil {
						continue
					}
					if strings.Compare(s, "Primary:CLEARTEXT") == 0 { //awwww yis
						//try decode the thing first
						nhex, err := hex.DecodeString(string(x.PropertyValue))
						if err != nil {
							continue
						}
						sdec, err := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().String(string(nhex))
						if err != nil {
							//check for machien key thingo here I guess
							continue
						}
						if !isASCII(sdec) {
							sdec = string(x.PropertyValue)
						}
						r.Username = username
						r.ClearPassword = sdec
					}

				}
			}

		}
	} else {
		fmt.Println("NOT VSS METHOD???")
	}

	//panic("lol")
	//*/
	return r, nil
}

func (g *Gosecretsdump) decryptHash(record esent.Esent_record) (dumpedHash, error) {
	d := dumpedHash{}
	if g.useVSSMethod {
		z := nToInternal["objectSid"]
		sid, err := SAMR_RPC_SID{}.Init(record.Column[z].BytVal)
		if err != nil {
			return d, err
		}
		d.Rid = sid.FormatCanonical()[strings.LastIndex(sid.FormatCanonical(), "-")+1:]

		//lm hash
		if record.Column[nToInternal["dBCSPwd"]].StrVal != "" {
			tmpLM := []byte{}
			encryptedLM := crypted_hash{}.Init(record.Column[nToInternal["dBCSPwd"]].BytVal)
			if bytes.Compare(encryptedLM.Header[:4], []byte("\x13\x00\x00\x00")) == 0 {
				encryptedLMW := crypted_hashw16{}.Init(record.Column[nToInternal["dBCSPwd"]].BytVal)
				pekIndex := encryptedLMW.Header
				tmpLM = decryptAES(g.pek[pekIndex[4]], encryptedLMW.EncrypedHash[:16], encryptedLMW.KeyMaterial[:])
			} else {
				tmpLM = g.removeRC4(encryptedLM)
			}
			d.LMHash = g.removeDES(tmpLM, d.Rid)
		} else {
			//hard coded empty lm hash
			d.LMHash, _ = hex.DecodeString("aad3b435b51404eeaad3b435b51404ee")
		}

		//nt hash
		if v := record.Column[nToInternal["unicodePwd"]].BytVal; len(v) > 0 {
			tmpNT := []byte{}
			encryptedNT := crypted_hash{}.Init(v)
			if bytes.Compare(encryptedNT.Header[:4], []byte("\x13\x00\x00\x00")) == 0 {
				encryptedNTW := crypted_hashw16{}.Init(record.Column[nToInternal["unicodePwd"]].BytVal)
				pekIndex := encryptedNTW.Header
				tmpNT = decryptAES(g.pek[pekIndex[4]], encryptedNTW.EncrypedHash[:16], encryptedNTW.KeyMaterial[:])
			} else {
				tmpNT = g.removeRC4(encryptedNT)
			}
			d.NTHash = g.removeDES(tmpNT, d.Rid)
		} else {
			//hard coded empty NTLM hash
			d.NTHash, _ = hex.DecodeString("31D6CFE0D16AE931B73C59D7E0C089C0")
		}

		//username
		if v := record.Column[nToInternal["userPrincipalName"]].StrVal; v != "" {
			rec := record.Column[nToInternal["userPrincipalName"]].StrVal
			recs := strings.Split(rec, "@")
			domain := recs[len(recs)-1]
			d.Username = fmt.Sprintf("%s\\%s", domain, record.Column[nToInternal["sAMAccountName"]].StrVal)
		} else {
			d.Username = fmt.Sprintf("%s", record.Column[nToInternal["sAMAccountName"]].StrVal)
		}

		if v := record.Column[nToInternal["userAccountControl"]].Long; v != 0 {
			d.UAC = decodeUAC(int(v))
		}
	} else {
		fmt.Println("DO NOT VSS METHOD?")
	}
	return d, nil
}

//https://stackoverflow.com/questions/53069040/checking-a-string-contains-only-ascii-characters
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > u.MaxASCII {
			return false
		}
	}
	return true
}

type uacFlags struct {
	Script, AccountDisable, HomeDirRequired,
	Lockout, PasswdNotReqd, EncryptedTextPwdAllowed,
	TempDupAccount, NormalAccount, InterDomainTrustAcct,
	WorkstationTrustAccount, ServerTrustAccount,
	DontExpirePassword, MNSLogonAccount, SmartcardRequired,
	TrustedForDelegation, NotDelegated, UseDESOnly,
	DontPreauth, PasswordExpired, TrustedToAuthForDelegation,
	PartialSecrets bool
}

//whoa this is a dumb way of doing it,
//but I've had too many rums to think of the actual way
func decodeUAC(val int) uacFlags {
	r := uacFlags{}
	r.Script = val|1 == val
	r.AccountDisable = val|2 == val
	r.HomeDirRequired = val|8 == val
	r.Lockout = val|6 == val
	r.PasswdNotReqd = val|32 == val
	r.EncryptedTextPwdAllowed = val|128 == val
	r.TempDupAccount = val|256 == val
	r.NormalAccount = val|512 == val
	r.InterDomainTrustAcct = val|2048 == val
	r.WorkstationTrustAccount = val|4096 == val
	r.ServerTrustAccount = val|8192 == val
	r.DontExpirePassword = val|65536 == val
	r.MNSLogonAccount = val|131072 == val
	r.SmartcardRequired = val|262144 == val
	r.TrustedForDelegation = val|524288 == val
	r.NotDelegated = val|1048576 == val
	r.UseDESOnly = val|2097152 == val
	r.DontPreauth = val|4194304 == val
	r.PasswordExpired = val|8388608 == val
	r.TrustedToAuthForDelegation = val|16777216 == val
	r.PartialSecrets = val|67108864 == val
	return r
}

func (g *Gosecretsdump) Dump() {
	//if local (always local for now)
	g.isRemote = false
	g.useVSSMethod = true
	if g.systemHiveLocation != "" {
		localOps := localOps{}.Init(g.systemHiveLocation)
		g.bootKey = localOps.getBootKey()
		if g.ntdsFileLocation != "" {
			g.noLMHash = localOps.checkNoLMHashPolicy()
		}
	}
	fmt.Println("Searching for pekList")
	g.GetPek()
	//verify pek retreived good
	if len(g.pek) < 1 {
		panic("NO PEK FOUND OK")
	}
	fmt.Println("Reading and decrypting hashes from", g.ntdsFileLocation)

	for {
		//read each record from the db
		record, err := g.db.GetNextRow(g.cursor)
		if err != nil {
			break //we will get an 'ignore' error when there are no more records
		}
		if _, ok := accTypes[record.Column[nToInternal["sAMAccountType"]].Long]; ok {
			//attempt decryption
			dh, err := g.decryptHash(record)
			if err != nil {
				fmt.Println("Coudln't decrypt record:", err.Error())
				continue
			}
			g.handleHash(dh)
			ds, err := g.decryptSupp(record)
			if err != nil {
				fmt.Println("Coudln't decrypt record:", err.Error())
				continue
			}
			g.handleSupp(dh, ds)
		}
	}
}

func (g Gosecretsdump) handleSupp(dh dumpedHash, ds suppInfo) {
	if ds.Username == "" {
		return
	}
	//print out the decrypted record
	prntLine := ds.HashString()
	if g.settings.Status {
		stat := "Enabled"
		if dh.UAC.AccountDisable {
			stat = "Disabled"
		}
		prntLine += " (status=" + stat + ")"
	}
	if g.settings.EnabledOnly {
		if !dh.UAC.AccountDisable {
			writeFileAndPrintLn(g.settings.Outfile, prntLine, !g.settings.NoPrint, true)
		}
	} else {
		writeFileAndPrintLn(g.settings.Outfile, prntLine, !g.settings.NoPrint, true)
	}
}

func (g Gosecretsdump) handleHash(dh dumpedHash) {
	//print out the decrypted record
	prntLine := dh.HashString()
	if g.settings.Status {
		stat := "Enabled"
		if dh.UAC.AccountDisable {
			stat = "Disabled"
		}
		prntLine += " (status=" + stat + ")"
	}
	if g.settings.EnabledOnly {
		if !dh.UAC.AccountDisable {
			writeFileAndPrintLn(g.settings.Outfile, prntLine, !g.settings.NoPrint, false)
		}
	} else {
		writeFileAndPrintLn(g.settings.Outfile, prntLine, !g.settings.NoPrint, false)
	}
}

func writeFileAndPrintLn(outfile, val string, print bool, cleartext bool) {
	if outfile != "" {
		if cleartext {
			outfile = outfile + ".cleartext"
		}
		file, err := os.OpenFile(outfile, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
		if err != nil {
			panic(err)
		}
		defer file.Close()
		file.WriteString(val + "\n")
		file.Sync()
	}
	if print {
		fmt.Println(val)
	}
}

type dumpedHash struct {
	Username string
	LMHash   []byte
	NTHash   []byte
	Rid      string
	Enabled  bool
	UAC      uacFlags
}

func (d dumpedHash) HashString() string {
	answer := fmt.Sprintf("%s:%s:%s:%s:::",
		d.Username, d.Rid,
		hex.EncodeToString(d.LMHash),
		hex.EncodeToString(d.NTHash))

	return answer
}

type peklist_enc struct {
	Header       [8]byte
	KeyMaterial  [16]byte
	EncryptedPek []byte // ":"
}

func (p peklist_enc) Init(data []byte) peklist_enc {
	r := peklist_enc{}
	//fill out all the things I guess
	lData := make([]byte, len(data)) //avoid mutation
	copy(lData, data)
	copy(r.Header[:], lData[:8])
	lData = lData[8:]
	copy(r.KeyMaterial[:], lData[:16])
	lData = lData[16:]
	r.EncryptedPek = make([]byte, len(lData))
	copy(r.EncryptedPek, lData)
	return r
}

type peklist_plain struct {
	Header       [32]byte
	DecryptedPek []byte // ":"
}

func (p peklist_plain) Init(data []byte) peklist_plain {
	r := peklist_plain{}
	lData := make([]byte, len(data))
	copy(lData, data)
	copy(r.Header[:], lData[:32])
	lData = lData[32:]
	r.DecryptedPek = make([]byte, len(lData))
	copy(r.DecryptedPek, lData)
	//fill out all the things I guess
	return r
}

type pek_key struct {
	Header  [1]byte
	Padding [3]byte
	Key     [16]byte
}

func (p pek_key) Init(data []byte) pek_key {
	r := pek_key{}
	lData := make([]byte, len(data))
	copy(lData, data)
	copy(r.Header[:], lData[:0])
	copy(r.Padding[:], lData[1:4])
	copy(r.Key[:], lData[4:20])
	return r
}
