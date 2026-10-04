//go:build darwin

package senclave

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// CoreFoundation, Security and LocalAuthentication constants used below.
const (
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt32Type   = 3

	accessControlUserPresence    = 1 << 0
	accessControlPrivateKeyUsage = 1 << 30

	osStatusInteractionNotAllowed = -25308
	osStatusUserCanceled          = -128
	osStatusMissingEntitlement    = -34018

	laErrorUserCancel      = -2
	laErrorBiometryLockout = -8
	laErrorNotInteractive  = -1004

	eciesAlgo = "kSecKeyAlgorithmECIESEncryptionCofactorVariableIVX963SHA256AESGCM"
)

// Key is a handle on a loaded Secure Enclave key.
type Key struct {
	blob []byte
	ref  uintptr // SecKeyRef, released by Close
}

// fw holds the runtime-bound framework entry points and constants.
type fw struct {
	cfStringCreateWithCString func(alloc uintptr, s string, enc uint32) uintptr
	cfStringGetLength         func(s uintptr) int
	cfStringGetCString        func(s uintptr, buf *byte, n int, enc uint32) bool
	cfDataCreate              func(alloc uintptr, b *byte, n int) uintptr
	cfDataGetLength           func(d uintptr) int
	cfDataGetBytePtr          func(d uintptr) uintptr
	cfDictionaryCreate        func(alloc uintptr, keys, vals *uintptr, n int, kcb, vcb uintptr) uintptr
	cfNumberCreate            func(alloc uintptr, typ int, v *int32) uintptr
	cfRelease                 func(ref uintptr)
	cfErrorGetCode            func(err uintptr) int
	cfErrorCopyDescription    func(err uintptr) uintptr

	secAccessControlCreateWithFlags  func(alloc, protection uintptr, flags uint64, err *uintptr) uintptr
	secKeyCreateRandomKey            func(params uintptr, err *uintptr) uintptr
	secKeyCopyPublicKey              func(key uintptr) uintptr
	secKeyCreateEncryptedData        func(key, alg, data uintptr, err *uintptr) uintptr
	secKeyCreateDecryptedData        func(key, alg, data uintptr, err *uintptr) uintptr
	secKeyCopyAttributes             func(key uintptr) uintptr
	secKeyCopyExternalRepresentation func(key uintptr, err *uintptr) uintptr
	secKeyCreateWithData             func(data, attrs uintptr, err *uintptr) uintptr
	cfDictionaryGetValue             func(dict, key uintptr) uintptr
	cfCopyDescription                func(ref uintptr) uintptr

	booleanTrue  uintptr
	booleanFalse uintptr
	dictKeyCB    uintptr
	dictValueCB  uintptr
	tokenOIDKey  uintptr
	consts       map[string]uintptr
}

var (
	loadOnce sync.Once
	loaded   *fw
	errLoad  error
)

// secConstNames lists the CFStringRef globals we dereference once.
var secConstNames = []string{
	"kSecAttrKeyType", "kSecAttrKeyTypeECSECPrimeRandom",
	"kSecAttrKeyClass", "kSecAttrKeyClassPrivate",
	"kSecAttrKeySizeInBits", "kSecAttrTokenID", "kSecAttrTokenIDSecureEnclave",
	"kSecPrivateKeyAttrs", "kSecAttrAccessControl", "kSecAttrIsPermanent",
	"kSecUseAuthenticationContext", "kSecValueData",
	"kSecAttrAccessibleWhenUnlockedThisDeviceOnly",
	eciesAlgo,
}

func load() (*fw, error) {
	loadOnce.Do(func() { loaded, errLoad = bind() })
	return loaded, errLoad
}

func openFramework(name string) (uintptr, error) {
	path := "/System/Library/Frameworks/" + name + ".framework/" + name
	lib, err := purego.Dlopen(path, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrUnsupported, name, err)
	}
	return lib, nil
}

func bind() (*fw, error) {
	cf, err := openFramework("CoreFoundation")
	if err != nil {
		return nil, err
	}
	sec, err := openFramework("Security")
	if err != nil {
		return nil, err
	}
	// Foundation and LocalAuthentication register the ObjC classes the
	// prompt reason needs (NSString, LAContext).
	for _, name := range []string{"Foundation", "LocalAuthentication"} {
		if _, err := openFramework(name); err != nil {
			return nil, err
		}
	}
	f := &fw{consts: make(map[string]uintptr, len(secConstNames))}
	purego.RegisterLibFunc(&f.cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&f.cfStringGetLength, cf, "CFStringGetLength")
	purego.RegisterLibFunc(&f.cfStringGetCString, cf, "CFStringGetCString")
	purego.RegisterLibFunc(&f.cfDataCreate, cf, "CFDataCreate")
	purego.RegisterLibFunc(&f.cfDataGetLength, cf, "CFDataGetLength")
	purego.RegisterLibFunc(&f.cfDataGetBytePtr, cf, "CFDataGetBytePtr")
	purego.RegisterLibFunc(&f.cfDictionaryCreate, cf, "CFDictionaryCreate")
	purego.RegisterLibFunc(&f.cfNumberCreate, cf, "CFNumberCreate")
	purego.RegisterLibFunc(&f.cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&f.cfErrorGetCode, cf, "CFErrorGetCode")
	purego.RegisterLibFunc(&f.cfErrorCopyDescription, cf, "CFErrorCopyDescription")

	purego.RegisterLibFunc(&f.secAccessControlCreateWithFlags, sec, "SecAccessControlCreateWithFlags")
	purego.RegisterLibFunc(&f.secKeyCreateRandomKey, sec, "SecKeyCreateRandomKey")
	purego.RegisterLibFunc(&f.secKeyCopyPublicKey, sec, "SecKeyCopyPublicKey")
	purego.RegisterLibFunc(&f.secKeyCreateEncryptedData, sec, "SecKeyCreateEncryptedData")
	purego.RegisterLibFunc(&f.secKeyCreateDecryptedData, sec, "SecKeyCreateDecryptedData")
	purego.RegisterLibFunc(&f.secKeyCopyAttributes, sec, "SecKeyCopyAttributes")
	purego.RegisterLibFunc(&f.secKeyCopyExternalRepresentation, sec, "SecKeyCopyExternalRepresentation")
	purego.RegisterLibFunc(&f.secKeyCreateWithData, sec, "SecKeyCreateWithData")
	purego.RegisterLibFunc(&f.cfDictionaryGetValue, cf, "CFDictionaryGetValue")
	purego.RegisterLibFunc(&f.cfCopyDescription, cf, "CFCopyDescription")

	// Struct globals are passed by address; CFTypeRef globals are
	// dereferenced once.
	if f.dictKeyCB, err = purego.Dlsym(cf, "kCFTypeDictionaryKeyCallBacks"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if f.dictValueCB, err = purego.Dlsym(cf, "kCFTypeDictionaryValueCallBacks"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	addr, err := purego.Dlsym(cf, "kCFBooleanTrue")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	f.booleanTrue = deref(addr)
	addr, err = purego.Dlsym(cf, "kCFBooleanFalse")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	f.booleanFalse = deref(addr)
	for _, name := range secConstNames {
		addr, err = purego.Dlsym(sec, name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrUnsupported, name, err)
		}
		f.consts[name] = deref(addr)
	}
	// The token object id attribute ("toid") carries an enclave key's
	// wrapped form. Its symbol is not in every SDK, so fall back to the
	// literal attribute name.
	if addr, err = purego.Dlsym(sec, "kSecAttrTokenOID"); err == nil {
		f.tokenOIDKey = deref(addr)
	} else {
		f.tokenOIDKey = f.cfStringCreateWithCString(0, "toid", cfStringEncodingUTF8)
	}
	return f, nil
}

func deref(addr uintptr) uintptr {
	return *(*uintptr)(unsafe.Pointer(addr)) //nolint:gosec,govet // reading a dlsym'd global CFTypeRef
}

// k returns a dereferenced Security constant.
func (f *fw) k(name string) uintptr { return f.consts[name] }

// data builds a CFData the caller must release.
func (f *fw) data(b []byte) uintptr {
	if len(b) == 0 {
		return f.cfDataCreate(0, nil, 0)
	}
	return f.cfDataCreate(0, &b[0], len(b))
}

// dict builds a CFDictionary from key/value pairs; caller releases it.
func (f *fw) dict(kv ...uintptr) uintptr {
	n := len(kv) / 2
	keys := make([]uintptr, n)
	vals := make([]uintptr, n)
	for i := range n {
		keys[i] = kv[2*i]
		vals[i] = kv[2*i+1]
	}
	return f.cfDictionaryCreate(0, &keys[0], &vals[0], n, f.dictKeyCB, f.dictValueCB)
}

func (f *fw) release(refs ...uintptr) {
	for _, r := range refs {
		if r != 0 {
			f.cfRelease(r)
		}
	}
}

// goString copies a CFString into a Go string.
func (f *fw) goString(s uintptr) string {
	if s == 0 {
		return ""
	}
	n := f.cfStringGetLength(s)*4 + 1
	buf := make([]byte, n)
	if !f.cfStringGetCString(s, &buf[0], n, cfStringEncodingUTF8) {
		return ""
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

// goBytes copies a CFData into a Go slice.
func (f *fw) goBytes(d uintptr) []byte {
	n := f.cfDataGetLength(d)
	if n == 0 {
		return nil
	}
	ptr := f.cfDataGetBytePtr(d)
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(ptr)), n)...) //nolint:gosec,govet // bounded by CFDataGetLength
}

// cfError converts a CFErrorRef into a Go error and releases it.
func (f *fw) cfError(ref uintptr, op string) error {
	if ref == 0 {
		return fmt.Errorf("secure enclave: %s failed", op)
	}
	code := f.cfErrorGetCode(ref)
	desc := f.cfErrorCopyDescription(ref)
	msg := f.goString(desc)
	f.release(desc, ref)
	if sentinel := mapCode(code); sentinel != nil {
		return fmt.Errorf("%w (%s: %s)", sentinel, op, msg)
	}
	return fmt.Errorf("secure enclave: %s: %s (code %d)", op, msg, code)
}

func mapCode(code int) error {
	switch code {
	case osStatusUserCanceled, laErrorUserCancel, laErrorBiometryLockout:
		return ErrCanceled
	case osStatusInteractionNotAllowed, laErrorNotInteractive:
		return ErrNoInteraction
	case osStatusMissingEntitlement:
		return ErrMissingEntitlement
	}
	return nil
}

// Generate creates a Secure Enclave key that needs user presence for every
// private-key operation and returns its exported blob. The blob is wrapped
// by this Mac's enclave: only this enclave can load it, and the access
// control travels inside it, so no process can use it without the prompt.
// The key is never added to the keychain, so no entitlement is required.
func Generate() ([]byte, error) {
	f, err := load()
	if err != nil {
		return nil, err
	}
	var cfErr uintptr
	access := f.secAccessControlCreateWithFlags(0, f.k("kSecAttrAccessibleWhenUnlockedThisDeviceOnly"),
		accessControlPrivateKeyUsage|accessControlUserPresence, &cfErr)
	if access == 0 {
		return nil, f.cfError(cfErr, "create access control")
	}
	// Not permanent: a keychain-resident key needs an entitlement.
	privAttrs := f.dict(
		f.k("kSecAttrIsPermanent"), f.booleanFalse,
		f.k("kSecAttrAccessControl"), access,
	)
	bits := int32(256)
	bitsNum := f.cfNumberCreate(0, cfNumberSInt32Type, &bits)
	attrs := f.dict(
		f.k("kSecAttrKeyType"), f.k("kSecAttrKeyTypeECSECPrimeRandom"),
		f.k("kSecAttrKeySizeInBits"), bitsNum,
		f.k("kSecAttrTokenID"), f.k("kSecAttrTokenIDSecureEnclave"),
		f.k("kSecPrivateKeyAttrs"), privAttrs,
	)
	defer f.release(attrs, bitsNum, privAttrs, access)

	cfErr = 0
	ref := f.secKeyCreateRandomKey(attrs, &cfErr)
	if ref == 0 {
		return nil, f.cfError(cfErr, "create key")
	}
	defer f.release(ref)
	// Enclave keys expose their wrapped form as the value-data attribute.
	attrsOut := f.secKeyCopyAttributes(ref)
	if attrsOut == 0 {
		return nil, errors.New("secure enclave: export key: no attributes")
	}
	defer f.release(attrsOut)
	blobRef := f.cfDictionaryGetValue(attrsOut, f.tokenOIDKey)
	if blobRef == 0 {
		return nil, errors.New("secure enclave: export key: no token object id")
	}
	return f.goBytes(blobRef), nil
}

// Load reconstructs the key from a blob produced by Generate.
func Load(blob []byte) (*Key, error) {
	f, err := load()
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, ErrNoKey
	}
	ref, err := f.importKey(blob, 0)
	if err != nil {
		return nil, err
	}
	return &Key{blob: blob, ref: ref}, nil
}

// importKey loads a blob, optionally bound to an authentication context.
func (f *fw) importKey(blob []byte, authCtx uintptr) (uintptr, error) {
	data := f.data(blob)
	// The token layer finds an existing enclave key by its object id;
	// without it, SecKeyCreateWithData would mint a fresh key instead.
	kv := []uintptr{
		f.k("kSecAttrKeyType"), f.k("kSecAttrKeyTypeECSECPrimeRandom"),
		f.k("kSecAttrKeyClass"), f.k("kSecAttrKeyClassPrivate"),
		f.k("kSecAttrTokenID"), f.k("kSecAttrTokenIDSecureEnclave"),
		f.tokenOIDKey, data,
	}
	if authCtx != 0 {
		kv = append(kv, f.k("kSecUseAuthenticationContext"), authCtx)
	}
	attrs := f.dict(kv...)
	defer f.release(attrs, data)
	var cfErr uintptr
	ref := f.secKeyCreateWithData(data, attrs, &cfErr)
	if ref == 0 {
		return 0, f.cfError(cfErr, "load key")
	}
	return ref, nil
}

// newAuthContext builds an LAContext carrying the dialog reason.
func newAuthContext(reason string) (uintptr, func(), error) {
	laClass := objc.GetClass("LAContext")
	nsString := objc.GetClass("NSString")
	if laClass == 0 || nsString == 0 {
		return 0, nil, fmt.Errorf("%w: LocalAuthentication classes unavailable", ErrUnsupported)
	}
	ctx := objc.ID(laClass).Send(objc.RegisterName("new"))
	if ctx == 0 {
		return 0, nil, fmt.Errorf("%w: LAContext init failed", ErrUnsupported)
	}
	cstr := append([]byte(reason), 0)
	str := objc.ID(nsString).Send(objc.RegisterName("stringWithUTF8String:"), unsafe.Pointer(&cstr[0])) //nolint:gosec // NUL-terminated buffer handed to NSString; ObjC copies it
	ctx.Send(objc.RegisterName("setLocalizedReason:"), str)
	release := func() { ctx.Send(objc.RegisterName("release")) }
	return uintptr(ctx), release, nil
}

// PublicKey returns the uncompressed X9.63 public point. No prompt.
func (k *Key) PublicKey() ([]byte, error) {
	f, err := load()
	if err != nil {
		return nil, err
	}
	pub := f.secKeyCopyPublicKey(k.ref)
	if pub == 0 {
		return nil, errors.New("secure enclave: no public key")
	}
	defer f.release(pub)
	var cfErr uintptr
	out := f.secKeyCopyExternalRepresentation(pub, &cfErr)
	if out == 0 {
		return nil, f.cfError(cfErr, "export public key")
	}
	defer f.release(out)
	return f.goBytes(out), nil
}

// Seal encrypts plaintext to the key's public half. No prompt.
func (k *Key) Seal(plaintext []byte) ([]byte, error) {
	f, err := load()
	if err != nil {
		return nil, err
	}
	pub := f.secKeyCopyPublicKey(k.ref)
	if pub == 0 {
		return nil, errors.New("secure enclave: no public key")
	}
	in := f.data(plaintext)
	defer f.release(in, pub)
	var cfErr uintptr
	out := f.secKeyCreateEncryptedData(pub, f.k(eciesAlgo), in, &cfErr)
	if out == 0 {
		return nil, f.cfError(cfErr, "seal")
	}
	defer f.release(out)
	return f.goBytes(out), nil
}

// Unseal decrypts ciphertext with the private key. macOS shows its
// authentication dialog with reason as the explanation.
func (k *Key) Unseal(ciphertext []byte, reason string) ([]byte, error) {
	f, err := load()
	if err != nil {
		return nil, err
	}
	ref := k.ref
	if reason != "" {
		// The reason rides on an LAContext bound at import time.
		authCtx, releaseCtx, err := newAuthContext(reason)
		if err != nil {
			return nil, err
		}
		defer releaseCtx()
		ref, err = f.importKey(k.blob, authCtx)
		if err != nil {
			return nil, err
		}
		defer f.release(ref)
	}
	in := f.data(ciphertext)
	defer f.release(in)
	var cfErr uintptr
	out := f.secKeyCreateDecryptedData(ref, f.k(eciesAlgo), in, &cfErr)
	if out == 0 {
		return nil, f.cfError(cfErr, "unseal")
	}
	defer f.release(out)
	return f.goBytes(out), nil
}

// Close releases the key handle.
func (k *Key) Close() {
	if k == nil || k.ref == 0 {
		return
	}
	if f, err := load(); err == nil {
		f.release(k.ref)
	}
	k.ref = 0
}
