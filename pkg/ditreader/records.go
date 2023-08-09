package ditreader

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lesnuages/gosecretsdump/pkg/esent"
	"golang.org/x/text/encoding/unicode"
)

func (d *DitReader) DecryptRecord(record esent.Esent_record) (DumpedHash, error) {
	dh := DumpedHash{}
	v, _ := record.GetBytVal(nobjectSid)
	sid, err := NewSAMRRPCSID(v) //record.Column[z].BytVal)
	if err != nil {
		return dh, err
	}
	//dh.Rid = sid.FormatCanonical()[strings.LastIndex(sid.FormatCanonical(), "-")+1:]
	dh.Rid = sid.Rid()

	//lm hash
	if b, err := record.GetBytVal(ndBCSPwd); err && len(b) > 0 {
		//if record.Column[ndBCSPwd"]].StrVal != "" {
		var tmpLM []byte
		encryptedLM, err := NewCryptedHash(b)
		if err != nil {
			return dh, err
		}
		if bytes.Equal(encryptedLM.Header[:4], []byte("\x13\x00\x00\x00")) {
			encryptedLMW := NewCryptedHashW16(b)
			pekIndex := encryptedLMW.Header
			tmpLM, err = DecryptAES(d.pek[pekIndex[4]], encryptedLMW.EncryptedHash[:16], encryptedLMW.KeyMaterial[:])
			if err != nil {
				return dh, err
			}
		} else {
			tmpLM, err = d.removeRC4(encryptedLM)
			if err != nil {
				return dh, err
			}
		}
		dh.LMHash, err = RemoveDES(tmpLM, dh.Rid)
		if err != nil {
			return dh, err
		}
	} else {
		//hard coded empty lm hash
		dh.LMHash = EmptyLM //, _ = hex.DecodeString("aad3b435b51404eeaad3b435b51404ee")
	}

	//nt hash
	if v, _ := record.GetBytVal(nunicodePwd); len(v) > 0 { //  record.Column[nunicodePwd"]].BytVal; len(v) > 0 {
		var tmpNT []byte
		encryptedNT, err := NewCryptedHash(v)
		if err != nil {
			return dh, err
		}
		if bytes.Equal(encryptedNT.Header[:4], []byte("\x13\x00\x00\x00")) {
			encryptedNTW := NewCryptedHashW16(v)
			pekIndex := encryptedNTW.Header
			tmpNT, err = DecryptAES(d.pek[pekIndex[4]], encryptedNTW.EncryptedHash[:16], encryptedNTW.KeyMaterial[:])
			if err != nil {
				return dh, err
			}
		} else {
			tmpNT, err = d.removeRC4(encryptedNT)
			if err != nil {
				return dh, err
			}
		}
		dh.NTHash, err = RemoveDES(tmpNT, dh.Rid)
		if err != nil {
			return dh, err
		}
	} else {
		//hard coded empty NTLM hash
		dh.NTHash = EmptyNT //, _ = hex.DecodeString("31D6CFE0D16AE931B73C59D7E0C089C0")
	}

	// account name
	account_name, _ := record.StrVal(nsAMAccountName)

	//username
	if v, err := record.StrVal(nuserPrincipalName); err == nil && v != "" {
		domain := v
		if pos := strings.LastIndex(domain, "@"); pos != -1 {
			domain = domain[pos+1:]
		}
		dh.Username = fmt.Sprintf("%s\\%s", domain, account_name)
	} else {
		dh.Username = account_name
	}

	//Password history LM
	if !d.noLMHash {
		if v, _ := record.GetBytVal(nlmPwdHistory); len(v) > 0 { //&& len(v) > 0 {
			ch, err := NewCryptedHash(v)
			if err != nil {
				return dh, err
			}
			var tmphst []byte
			tmphst, err = d.removeRC4(ch)
			if err != nil {
				return dh, err
			}

			for i := 16; i < len(tmphst); i += 16 {
				hst1 := tmphst[i : i+16]
				hst2, err := RemoveDES(hst1, dh.Rid)
				dh.History.LmHist = append(dh.History.LmHist, hst2)
				if err != nil {
					return dh, err
				}
			}
		}
	}

	//password history NT
	if v, _ := record.GetBytVal(nntPwdHistory); len(v) > 0 { //&& len(v) > 0 {
		ch, err := NewCryptedHash(v)
		if err != nil {
			return dh, err
		}
		var tmphst []byte
		if bytes.Equal(ch.Header[:4], []byte("\x13\x00\x00\x00")) {
			encryptedNTW := NewCryptedHashW16History(v)
			pekIndex := encryptedNTW.Header
			tmphst, err = DecryptAES(d.pek[pekIndex[4]], encryptedNTW.EncryptedHash[:], encryptedNTW.KeyMaterial[:])
			if err != nil {
				return dh, err
			}
		} else {
			tmphst, err = d.removeRC4(ch)
			if err != nil {
				return dh, err
			}
		}
		for i := 16; i < len(tmphst); i += 16 {
			hst1 := tmphst[i : i+16]
			hst2, err := RemoveDES(hst1, dh.Rid)
			if err != nil {
				return dh, err
			}
			dh.History.NTHist = append(dh.History.NTHist, hst2)
		}

	}
	//check if account is enabled
	if v, _ := record.GetLongVal(nuserAccountControl); v != 0 { // record.Column[nuserAccountControl"]].Long; v != 0 {
		dh.UAC = decodeUAC(int(v))
	}

	//check if cleartext exists
	if val, _ := record.GetBytVal(nsupplementalCredentials); len(val) > 24 {
		//if val := record.Column[nsupplementalCredentials"]]; len(val.BytVal) > 24 {
		var err error
		dh.Supp, err = d.decryptSupp(record)
		if err != nil {
			fmt.Println("Error: ", err)
		}
	}

	return dh, nil
}

func (d DitReader) decryptSupp(record esent.Esent_record) (SuppInfo, error) {
	r := SuppInfo{}

	bval, _ := record.GetBytVal(nsupplementalCredentials) // record.Column[nsupplementalCredentials"]]
	if len(bval) > 24 {                                   //is the value above the minimum for plaintex passwords?
		username, _ := record.StrVal(nsAMAccountName)
		var plainBytes []byte
		//check if the record is something something? has a UPN?
		if v, _ := record.StrVal(nuserPrincipalName); v != "" { //record.Column[nuserPrincipalName"]].StrVal != "" {
			domain := v
			parts := strings.Split(domain, "@")
			domain = parts[len(parts)-1]
			username = fmt.Sprintf("%s\\%s", domain, username)
		}
		//fmt.Println(val.BytVal)
		ct, err := NewCryptedHash(bval)
		if err != nil {
			return r, err
		}
		//ct := crypted_hash{}.Init(val.BytVal)

		//check for windows 2016 tp4
		if bytes.Equal(ct.Header[:4], []byte{0x13, 0, 0, 0}) {
			//fmt.Println("TODO: WINDOWS 2016 SUPP DATA FOR PLAINTEXT")
			pekIndex := binary.LittleEndian.Uint16(ct.Header[4:6])
			plainBytes, err = DecryptAES(d.pek[pekIndex],
				ct.EncryptedHash[4:],
				ct.KeyMaterial[:])
			if err != nil {
				return r, err
			}
		} else {
			plainBytes, err = d.removeRC4(ct)
			if err != nil {
				return r, err
			}
		}
		if len(plainBytes) < 100 {
			return r, fmt.Errorf("bad length for user properties: expecting >100 got %d ", len(plainBytes))
		}
		props := NewSAMRUserProperties(plainBytes)

		for _, x := range props.Properties {
			//apparently we should care about kerberos-newer-keys, but I don't really want to at the moment
			s, e := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder().String(string(x.PropertyName))
			if e != nil {
				continue
			}
			if strings.Compare(s, "Primary:Kerberos-Newer-Keys") == 0 {
				//try decode the thing first
				nhex, err := hex.DecodeString(string(x.PropertyValue))
				if err != nil {
					continue
				}
				cursor := 0
				rec := NewSAMRKerbStoredCredNew(nhex)
				r.KerbKeys = make([]string, rec.CredentialCount)
				for credIndex := uint16(0); credIndex < rec.CredentialCount; credIndex++ {
					keyData := NewSAMRKerbKeyDataNew(rec.Buffer[cursor:])
					cursor += 24 //sizeof samrkerbkeydatanew
					keyVal := nhex[keyData.KeyOffset : keyData.KeyOffset+keyData.KeyLength]
					if k, ok := kerbkeytype[keyData.KeyType]; ok {
						r.KerbKeys[credIndex] = fmt.Sprintf("%s:%s:%s", username, k, hex.EncodeToString(keyVal))
					} else {
						r.KerbKeys[credIndex] = fmt.Sprintf("%s:%d:%s", username, keyData.KeyType, hex.EncodeToString(keyVal))
					}
				}
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
					r.NotASCII = true
				}
				r.ClearPassword = sdec
			}
			r.Username = username
		}
	}

	return r, nil
}
