package collector

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"syscall"
)

const (
	// maxCertificateFile bounds the size of a certificate file. Larger
	// files are skipped with a warning.
	maxCertificateFile = 1 << 20
	// maxCertificatesPerFile bounds the certificates kept from one file
	// (a leaf and its chain, or a CA bundle).
	maxCertificatesPerFile = 32
	// maxSymlinkHops bounds the symbolic links followed for one path.
	maxSymlinkHops = 16
	// maxCachedCertificateFiles bounds the parsed-file cache.
	maxCachedCertificateFiles = 8192
)

var (
	errCertificateFileTooLarge = errors.New("larger than 1 MiB")
	errNotRegularFile          = errors.New("not a regular file")
	errTooManySymlinks         = errors.New("too many levels of symbolic links")
	errFileChanged             = errors.New("changed while reading")
)

// symlinkEscapeError reports a symbolic link in a user-owned tree whose
// target lies outside that tree.
type symlinkEscapeError struct {
	link, target, root string
}

func (err *symlinkEscapeError) Error() string {
	return fmt.Sprintf("not following symbolic link %s -> %s out of %s", err.link, err.target, err.root)
}

// scanCertificatePEM returns the DER bytes of every CERTIFICATE block in
// data, in file order, at most maxCertificatesPerFile. Lines of any other
// block, such as a private key in a combined file, are skipped while
// scanning: they are never decoded or copied.
func scanCertificatePEM(data []byte) [][]byte {
	const (
		outside = iota
		inCertificate
		inOther
	)
	var blocks [][]byte
	var body []byte
	state := outside
	for len(data) > 0 && len(blocks) < maxCertificatesPerFile {
		var line []byte
		line, data, _ = bytes.Cut(data, []byte{'\n'})
		line = bytes.TrimSpace(line)
		if state == inCertificate {
			switch {
			case string(line) == "-----END CERTIFICATE-----":
				if der, err := base64.StdEncoding.AppendDecode(nil, body); err == nil && len(der) > 0 {
					blocks = append(blocks, der)
				}
				state = outside
				continue
			case bytes.HasPrefix(line, []byte("-----")):
				// A malformed block: start over with this line.
				state = outside
			case bytes.IndexByte(line, ':') >= 0:
				// An RFC 1421 header; certificates have none.
				continue
			default:
				body = append(body, line...)
				continue
			}
		}
		if state == inOther {
			if bytes.HasPrefix(line, []byte("-----END ")) {
				state = outside
			}
			continue
		}
		label, ok := bytes.CutPrefix(line, []byte("-----BEGIN "))
		if !ok {
			continue
		}
		if string(label) == "CERTIFICATE-----" {
			state = inCertificate
			body = body[:0]
		} else {
			state = inOther
		}
	}
	return blocks
}

// parseCertificateBlocks parses DER certificates, skipping invalid ones.
func parseCertificateBlocks(blocks [][]byte) []*x509.Certificate {
	var certificates []*x509.Certificate
	for _, der := range blocks {
		if certificate, err := x509.ParseCertificate(der); err == nil {
			certificates = append(certificates, certificate)
		}
	}
	return certificates
}

// fingerprintOf is the SHA-256 of a certificate's DER, lowercase hex.
func fingerprintOf(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:])
}

// fileOwner returns the owner of a file when the file system exposes it.
func fileOwner(info fs.FileInfo) (uint32, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid, true
	}
	return 0, false
}

// userTreePrefixes are the trees that belong to hosting users even where
// the file system does not expose ownership: /home/<user>, /srv/<site>,
// /var/www/vhosts/<subscription> (Plesk) and /var/www/<site>.
var userTreePrefixes = []string{"/home/", "/srv/", "/var/www/vhosts/", "/var/www/"}

// userRoot returns the outermost directory at or above dir that belongs to
// a user (owned by a non-root uid, or one of userTreePrefixes' entries), or
// "" when dir is in a system tree.
func (host *Host) userRoot(dir string) string {
	dir = path.Clean("/" + dir)
	byPrefix := ""
	for _, prefix := range userTreePrefixes {
		rest, ok := strings.CutPrefix(dir+"/", prefix)
		if !ok || rest == "" {
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		byPrefix = prefix + name
		break
	}
	current := ""
	for _, component := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if component == "" {
			break
		}
		current += "/" + component
		if byPrefix != "" && len(current) >= len(byPrefix) {
			return byPrefix
		}
		info, err := fs.Lstat(host.FS, relative(current))
		if err != nil {
			break
		}
		if uid, known := fileOwner(info); known && uid != 0 {
			return current
		}
	}
	return byPrefix
}

func within(name, root string) bool {
	return name == root || strings.HasPrefix(name, root+"/")
}

// resolvePath follows the symbolic links in name component by component,
// at most maxSymlinkHops. A link inside a user-owned tree (userRoot) is
// only followed when the file it ends at lies inside that tree as well, so
// a hosting user cannot point the agent at files elsewhere. It returns the
// resolved path and its (non-link) FileInfo.
func (host *Host) resolvePath(name string) (string, fs.FileInfo, error) {
	pending := strings.Split(strings.TrimPrefix(path.Clean("/"+name), "/"), "/")
	resolved := "/"
	var info fs.FileInfo
	var confined []*symlinkEscapeError
	hops := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, component)
		var err error
		info, err = fs.Lstat(host.FS, relative(next))
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			if len(pending) > 0 && !info.IsDir() {
				return "", nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}
			resolved = next
			continue
		}
		if hops++; hops > maxSymlinkHops {
			return "", nil, &fs.PathError{Op: "open", Path: name, Err: errTooManySymlinks}
		}
		target, err := fs.ReadLink(host.FS, relative(next))
		if err != nil {
			return "", nil, err
		}
		if root := host.userRoot(resolved); root != "" {
			confined = append(confined, &symlinkEscapeError{link: next, target: target, root: root})
		}
		if path.IsAbs(target) {
			resolved = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	for _, link := range confined {
		if !within(resolved, link.root) {
			return "", nil, link
		}
	}
	if info == nil {
		return "", nil, &fs.PathError{Op: "open", Path: name, Err: errNotRegularFile}
	}
	return resolved, info, nil
}

// readBoundedFile reads a regular file of at most limit bytes after
// resolving its symbolic links with resolvePath's policy.
func (host *Host) readBoundedFile(name string, limit int64) ([]byte, error) {
	resolved, info, err := host.resolvePath(name)
	if err != nil {
		return nil, err
	}
	return host.readResolved(resolved, info, limit)
}

func (host *Host) readResolved(resolved string, info fs.FileInfo, limit int64) ([]byte, error) {
	if !info.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	if info.Size() > limit {
		if limit == maxCertificateFile {
			return nil, errCertificateFileTooLarge
		}
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	file, err := host.FS.Open(relative(resolved))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// One byte more than the size seen tells that the file grew.
	buffer := make([]byte, info.Size()+1)
	read, err := io.ReadFull(file, buffer)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
	case err != nil:
		clear(buffer)
		return nil, err
	default:
		clear(buffer)
		return nil, errFileChanged
	}
	return buffer[:read], nil
}

// certificateFile is the parsed content of one certificate file: its
// certificates in file order, leaf first. Nothing else from the file is
// kept.
type certificateFile struct {
	stamp        string
	certificates []*x509.Certificate
	err          error
}

// leaf returns the file's first certificate, or nil.
func (file *certificateFile) leaf() *x509.Certificate {
	if file == nil || len(file.certificates) == 0 {
		return nil
	}
	return file.certificates[0]
}

// readCertificateFile reads name with the symlink and size policy and
// keeps only its certificates. The read buffer, which may hold a private
// key, is cleared before returning.
func (host *Host) readCertificateFile(name string) *certificateFile {
	resolved, info, err := host.resolvePath(name)
	if err != nil {
		return &certificateFile{err: err}
	}
	return host.readCertificateResolved(resolved, info)
}

func (host *Host) readCertificateResolved(resolved string, info fs.FileInfo) *certificateFile {
	data, err := host.readResolved(resolved, info, maxCertificateFile)
	if err != nil {
		return &certificateFile{err: err}
	}
	defer clear(data)
	return &certificateFile{
		stamp:        fileStamp(resolved, info),
		certificates: parseCertificateBlocks(scanCertificatePEM(data)),
	}
}

// fileStamp identifies a file's version: path, size, mtime and inode.
func fileStamp(resolved string, info fs.FileInfo) string {
	inode := uint64(0)
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		inode = uint64(stat.Ino)
	}
	return fmt.Sprintf("%s\x00%d\x00%d\x00%d", resolved, info.Size(), info.ModTime().UnixNano(), inode)
}

// certificateStore caches parsed certificate files across collections and
// collectors, so the vhosts and certificates datasets share one parse. A
// file is re-read only when its resolved path, size, mtime or inode
// changed.
type certificateStore struct {
	mu    sync.Mutex
	files map[certificateStoreKey]*certificateFile
}

type certificateStoreKey struct {
	host *Host
	name string
}

func newCertificateStore() *certificateStore {
	return &certificateStore{files: make(map[certificateStoreKey]*certificateFile)}
}

// load returns the parsed file. Errors (missing, too large, an escaping
// link) are in certificateFile.err. A nil store reads without caching.
func (store *certificateStore) load(host *Host, name string) *certificateFile {
	resolved, info, err := host.resolvePath(name)
	if err != nil {
		return &certificateFile{err: err}
	}
	if store == nil {
		return host.readCertificateResolved(resolved, info)
	}
	key := certificateStoreKey{host: host, name: name}
	stamp := fileStamp(resolved, info)
	store.mu.Lock()
	cached, ok := store.files[key]
	store.mu.Unlock()
	if ok && cached.stamp == stamp {
		return cached
	}
	file := host.readCertificateResolved(resolved, info)
	if file.err != nil {
		return file
	}
	store.mu.Lock()
	if len(store.files) >= maxCachedCertificateFiles {
		clear(store.files)
	}
	store.files[key] = file
	store.mu.Unlock()
	return file
}

// certificateWarning describes a load error worth logging, or "" for
// missing or unreadable files, which are normal (a configured but absent
// snakeoil file, an unprivileged collect).
func certificateWarning(name string, err error) string {
	if err == nil || ignorable(err) {
		return ""
	}
	var escape *symlinkEscapeError
	if errors.As(err, &escape) {
		return name + ": " + escape.Error()
	}
	return name + ": skipped, " + err.Error()
}
