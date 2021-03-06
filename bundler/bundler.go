// package bundler implements certificate bundling functionality for CF-SSL.
package bundler

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/errors"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/ubiquity"
)

// intermediateStash contains the path to the directory where
// downloaded intermediates should be saved.
var IntermediateStash = "intermediates"

// BundleFlavor is named optimization strategy on certificate chain selection when bundling.
type BundleFlavor string

const (
	Optimal    BundleFlavor = "optimal"    // Optimal means the shortest chain with newest intermediates and the most advanced crypto.
	Ubiquitous BundleFlavor = "ubiquitous" // Ubiquitous is aimed to provide the chain which is accepted by the most platforms.
)

const (
	sha2Warning          = "The bundle contains certs signed with advanced hash functions such as SHA2,  which are problematic at certain operating systems, e.g. Windows XP SP2."
	expiringWarningStub  = "The bundle is expiring within 30 days. "
	untrustedWarningStub = "The bundle may not be trusted by the following platform(s):"
)

// A Bundler contains the certificate pools for producing certificate
// bundles. It contains any intermediates and root certificates that
// should be used.
type Bundler struct {
	RootPool         *x509.CertPool
	IntermediatePool *x509.CertPool
	KnownIssuers     map[string]bool
}

// NewBundler creates a new Bundler from the files passed in; these
// files should contain a list of valid root certificates and a list
// of valid intermediate certificates, respectively.
func NewBundler(caBundleFile, intBundleFile string) (*Bundler, error) {
	log.Debug("Loading CA bundle: ", caBundleFile)
	caBundlePEM, err := ioutil.ReadFile(caBundleFile)
	if err != nil {
		log.Errorf("root bundle failed to load: %v", err)
		return nil, errors.New(errors.RootError, errors.None, err)
	}

	log.Debug("Loading Intermediate bundle: ", intBundleFile)
	intBundlePEM, err := ioutil.ReadFile(intBundleFile)
	if err != nil {
		log.Errorf("intermediate bundle failed to load: %v", err)
		return nil, errors.New(errors.IntermediatesError, errors.None, err)
	}

	if _, err := os.Stat(IntermediateStash); err != nil && os.IsNotExist(err) {
		log.Infof("intermediate stash directory %s doesn't exist, creating", IntermediateStash)
		err = os.MkdirAll(IntermediateStash, 0755)
		if err != nil {
			log.Errorf("failed to create intermediate stash directory %s: %v", err)
			return nil, err
		}
		log.Infof("intermediate stash directory %s created", IntermediateStash)
	}
	return NewBundlerFromPEM(caBundlePEM, intBundlePEM)
}

// NewBundlerFromPEM creates a new Bundler from PEM-encoded root certificates and
// intermediate certificates.
func NewBundlerFromPEM(caBundlePEM, intBundlePEM []byte) (*Bundler, error) {
	b := &Bundler{
		RootPool:         x509.NewCertPool(),
		IntermediatePool: x509.NewCertPool(),
		KnownIssuers:     map[string]bool{},
	}

	log.Debug("parsing root certificates from PEM")
	roots, err := helpers.ParseCertificatesPEM(caBundlePEM)
	if err != nil {
		log.Errorf("failed to parse root bundle: %v", err)
		return nil, errors.New(errors.RootError, errors.None, err)
	}

	log.Debug("parse intermediate certificates from PEM")
	var intermediates []*x509.Certificate
	if intermediates, err = helpers.ParseCertificatesPEM(intBundlePEM); err != nil {
		log.Errorf("failed to parse intermediate bundle: %v", err)
		return nil, errors.New(errors.IntermediatesError, errors.None, err)
	}

	log.Debug("building certificate pools")
	for _, c := range roots {
		b.RootPool.AddCert(c)
		b.KnownIssuers[string(c.Signature)] = true
	}

	for _, c := range intermediates {
		b.IntermediatePool.AddCert(c)
		b.KnownIssuers[string(c.Signature)] = true
	}

	log.Debug("bundler set up")
	return b, nil
}

// VerifyOptions generates an x509 VerifyOptions structure that can be
// used for verifying certificates.
func (b *Bundler) VerifyOptions() x509.VerifyOptions {
	return x509.VerifyOptions{
		Roots:         b.RootPool,
		Intermediates: b.IntermediatePool,
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageMicrosoftServerGatedCrypto,
			x509.ExtKeyUsageNetscapeServerGatedCrypto,
		},
	}
}

// BundleFromFile takes a set of files containing the PEM-encoded leaf certificate
// (optionally along with some intermediate certs), the PEM-encoded private key
// and returns the bundle built from that key and the certificate(s).
func (b *Bundler) BundleFromFile(bundleFile, keyFile string, flavor BundleFlavor) (*Bundle, error) {
	log.Debug("Loading Certificate: ", bundleFile)
	certsPEM, err := ioutil.ReadFile(bundleFile)
	if err != nil {
		return nil, errors.New(errors.CertificateError, errors.ReadFailed, err)
	}

	var keyPEM []byte = nil
	// Load private key PEM only if a file is given
	if keyFile != "" {
		log.Debug("Loading private key: ", keyFile)
		keyPEM, err = ioutil.ReadFile(keyFile)
		if err != nil {
			log.Debugf("failed to read private key: ", err)
			return nil, errors.New(errors.PrivateKeyError, errors.ReadFailed, err)
		}
		if len(keyPEM) == 0 {
			log.Debug("key is empty")
			return nil, errors.New(errors.PrivateKeyError, errors.DecodeFailed, err)
		}
	}

	return b.BundleFromPEM(certsPEM, keyPEM, flavor)
}

// BundleFromPEM builds a certificate bundle from the set of byte
// slices containing the PEM-encoded certificate(s), private key.
func (b *Bundler) BundleFromPEM(certsPEM, keyPEM []byte, flavor BundleFlavor) (*Bundle, error) {
	log.Debug("bundling from PEM files")
	var key interface{}
	var err error
	if len(keyPEM) != 0 {
		key, err = helpers.ParsePrivateKeyPEM(keyPEM)
		log.Debugf("failed to parse private key: %v", err)
		if err != nil {
			return nil, err
		}
	}

	certs, err := helpers.ParseCertificatesPEM(certsPEM)
	if err != nil {
		log.Debugf("failed to parse certificates: %v", err)
		return nil, err
	} else if len(certs) == 0 {
		log.Debugf("no certificates found")
		return nil, errors.New(errors.CertificateError, errors.DecodeFailed, nil)
	}

	log.Debugf("bundle ready")
	return b.Bundle(certs, key, flavor)
}

// BundleFromRemote fetches the certificate chain served by the server at
// serverName (or ip, if the ip argument is not the empty string). It
// is expected that the method will be able to make a connection at
// port 443. The chain used by the server in this connection is
// used to rebuild the bundle.
func (b *Bundler) BundleFromRemote(serverName, ip string) (*Bundle, error) {
	config := &tls.Config{
		RootCAs:    b.RootPool,
		ServerName: serverName,
	}

	// Dial by IP if present
	var dialName string
	if ip != "" {
		dialName = ip + ":443"
	} else {
		dialName = serverName + ":443"
	}

	log.Debugf("bundling from remote %s", dialName)
	conn, err := tls.Dial("tcp", dialName, config)
	var dialError string
	// If there's an error in tls.Dial, try again with
	// InsecureSkipVerify to fetch the remote bundle to (re-)bundle with.
	// If the bundle is indeed not usable (expired, mismatched hostnames, etc.),
	// report the error.
	// Otherwise, create a working bundle and insert the tls error in the bundle.Status.
	if err != nil {
		log.Debugf("dial failed: %v", err)
		// record the error msg
		dialError = fmt.Sprintf("Failed rigid TLS handshake with %s: %v", dialName, err)
		// dial again with InsecureSkipVerify
		log.Debugf("try again with InsecureSkipVerify.")
		config.InsecureSkipVerify = true
		conn, err = tls.Dial("tcp", dialName, config)
		if err != nil {
			log.Debugf("dial with InsecureSkipVerify failed: %v", err)
			return nil, errors.New(errors.DialError, errors.Unknown, err)
		}
	}

	connState := conn.ConnectionState()

	certs := connState.PeerCertificates

	err = conn.VerifyHostname(serverName)
	if err != nil {
		log.Debugf("failed to verify hostname: %v", err)
		return nil, errors.New(errors.CertificateError, errors.VerifyFailed, err)
	}
	// verify peer intermediates and store them if there is any missing from the bundle.
	// Don't care if there is error, will throw it any way in Bundle() call.
	b.fetchIntermediates(certs)

	// Bundle with remote certs. Inject the initial dial error, if any, to the status reporting.
	bundle, err := b.Bundle(certs, nil, Ubiquitous)
	if err != nil {
		return nil, err
	} else if dialError != "" {
		bundle.Status.Messages = append(bundle.Status.Messages, dialError)
	}
	return bundle, err
}

type fetchedIntermediate struct {
	Cert *x509.Certificate
	Name string
}

// fetchRemoteCertificate retrieves a single URL pointing to a certificate
// and attempts to first parse it as a DER-encoded certificate; if
// this fails, it attempts to decode it as a PEM-encoded certificate.
func fetchRemoteCertificate(certURL string) (fi *fetchedIntermediate, err error) {
	log.Debugf("fetching remote certificate: %s", certURL)
	var resp *http.Response
	resp, err = http.Get(certURL)
	if err != nil {
		log.Debugf("failed HTTP get: %v", err)
		return
	}

	defer resp.Body.Close()
	var certData []byte
	certData, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("failed to read response body: %v", err)
		return
	}

	log.Debugf("attempting to parse certificate as DER")
	crt, err := x509.ParseCertificate(certData)
	if err != nil {
		log.Debugf("attempting to parse certificate as PEM")
		crt, err = helpers.ParseCertificatePEM(certData)
		if err != nil {
			log.Debugf("failed to parse certificate: %v", err)
			return
		}
	}

	log.Debugf("certificate fetch succeeds")
	fi = &fetchedIntermediate{crt, filepath.Base(certURL)}
	return
}

func isSelfSigned(cert *x509.Certificate) bool {
	return cert.CheckSignatureFrom(cert) == nil
}

func isChainRootNode(cert *x509.Certificate) bool {
	if isSelfSigned(cert) {
		return true
	}
	return false
}

func (b *Bundler) verifyChain(chain []*fetchedIntermediate) bool {
	// This process will verify if the root of the (partial) chain is in our root pool,
	// and will fail otherwise.
	log.Debugf("verifying chain")
	for vchain := chain[:]; len(vchain) > 0; vchain = vchain[1:] {
		cert := vchain[0]
		// If this is a certificate in one of the pools, skip it.
		if b.KnownIssuers[string(cert.Cert.Signature)] {
			log.Debugf("certificate is known")
			continue
		}

		_, err := cert.Cert.Verify(b.VerifyOptions())
		if err != nil {
			log.Debugf("certificate failed verification: %v", err)
			return false
		} else if len(chain) == len(vchain) && isChainRootNode(cert.Cert) {
			// The first certificate in the chain is a root; it shouldn't be stored.
			log.Debug("looking at root certificate, will not store")
			continue
		}

		log.Debugf("add certificate to intermediate pool")
		b.IntermediatePool.AddCert(cert.Cert)
		if cert.Name == "" {
			continue
		}
		fileName := filepath.Join(IntermediateStash, cert.Name)
		fileName += fmt.Sprintf(".%d", time.Now().UnixNano())

		b.KnownIssuers[string(cert.Cert.Signature)] = true

		var block = pem.Block{Type: "CERTIFICATE", Bytes: cert.Cert.Raw}

		log.Debugf("write intermediate to stash directory: %s", fileName)
		// If the write fails, verification should not fail.
		err = ioutil.WriteFile(fileName, pem.EncodeToMemory(&block), 0644)
		if err != nil {
			log.Errorf("failed to write new intermediate: %v", err)
		} else {
			log.Info("stashed new intermediate ", cert.Name)
		}
	}
	return true
}

// fetchIntermediates goes through each of the URLs in the AIA "Issuing
// CA" extensions and fetches those certificates. If those
// certificates are not present in either the root pool or
// intermediate pool, the certificate is saved to file and added to
// the list of intermediates to be used for verification. This will
// not add any new certificates to the root pool; if the ultimate
// issuer is not trusted, fetching the certicate here will not change
// that.
func (b *Bundler) fetchIntermediates(certs []*x509.Certificate) (err error) {
	log.Debugf("searching intermediates")
	if _, err := os.Stat(IntermediateStash); err != nil && os.IsNotExist(err) {
		log.Infof("intermediate stash directory %s doesn't exist, creating", IntermediateStash)
		err = os.MkdirAll(IntermediateStash, 0755)
		if err != nil {
			log.Errorf("failed to create intermediate stash directory %s: %v", err)
			return err
		}
		log.Infof("intermediate stash directory %s created", IntermediateStash)
	}

	// stores URLs and certificate signatures that have been seen
	seen := map[string]bool{}
	var foundChains int

	// Construct a verify chain as a reversed partial bundle,
	// such that the certs are ordered by promxity to the root CAs.
	var chain []*fetchedIntermediate
	for i, cert := range certs {
		var name string
		// Construct file name for non-leaf certs so they can be saved to disk.
		if i > 0 {
			// construct the filename as the CN with no period and space
			name = strings.Replace(cert.Subject.CommonName, ".", "", -1)
			name = strings.Replace(name, " ", "", -1)
			name += ".crt"
		}
		chain = append([]*fetchedIntermediate{{cert, name}}, chain...)
		seen[string(cert.Signature)] = true
	}

	// Verify the chain and store valid intermediates in the chain.
	// If it doesn't verify, fetch the intermediates and extend the chain
	// in a DFS manner and verify each time we hit a root.
	for {
		if len(chain) == 0 {
			log.Debugf("search complete")
			if foundChains == 0 {
				return x509.UnknownAuthorityError{}
			}
			return nil
		}

		current := chain[0]
		var advanced bool
		if b.verifyChain(chain) {
			foundChains++
		} else {
			log.Debugf("walk AIA issuers")
			for _, url := range current.Cert.IssuingCertificateURL {
				if seen[url] {
					log.Debugf("url %s has been seen", url)
					continue
				}
				crt, err := fetchRemoteCertificate(url)
				if err != nil {
					continue
				} else if seen[string(crt.Cert.Signature)] {
					log.Debugf("fetched certificate is known")
					continue
				}
				seen[url] = true
				seen[string(crt.Cert.Signature)] = true
				chain = append([]*fetchedIntermediate{crt}, chain...)
				advanced = true
				break
			}
		}

		if !advanced {
			log.Debugf("didn't advance, stepping back")
			chain = chain[1:]
		}
	}
}

// Bundle takes an X509 certificate (already in the
// Certificate structure), a private key in one of the appropriate
// formats (i.e. *rsa.PrivateKey or *ecdsa.PrivateKey), using them to
// build a certificate bundle.
func (b *Bundler) Bundle(certs []*x509.Certificate, key interface{}, flavor BundleFlavor) (*Bundle, error) {
	log.Infof("bundling certificate for %+v", certs[0].Subject)
	if len(certs) == 0 {
		return nil, nil
	}
	var ok bool
	cert := certs[0]
	if key != nil {
		switch {
		case cert.PublicKeyAlgorithm == x509.RSA:
			var rsaKey *rsa.PrivateKey
			if rsaKey, ok = key.(*rsa.PrivateKey); !ok {
				return nil, errors.New(errors.PrivateKeyError, errors.KeyMismatch, nil)
			}
			if cert.PublicKey.(*rsa.PublicKey).N.Cmp(rsaKey.PublicKey.N) != 0 {
				return nil, errors.New(errors.PrivateKeyError, errors.KeyMismatch, nil)
			}
		case cert.PublicKeyAlgorithm == x509.ECDSA:
			var ecdsaKey *ecdsa.PrivateKey
			if ecdsaKey, ok = key.(*ecdsa.PrivateKey); !ok {
				return nil, errors.New(errors.PrivateKeyError, errors.KeyMismatch, nil)
			}
			if cert.PublicKey.(*ecdsa.PublicKey).X.Cmp(ecdsaKey.PublicKey.X) != 0 {
				return nil, errors.New(errors.PrivateKeyError, errors.KeyMismatch, nil)
			}
		default:
			return nil, errors.New(errors.PrivateKeyError, errors.NotRSAOrECC, nil)
		}
	} else {
		switch {
		case cert.PublicKeyAlgorithm == x509.RSA:
		case cert.PublicKeyAlgorithm == x509.ECDSA:
		default:
			return nil, errors.New(errors.PrivateKeyError, errors.NotRSAOrECC, nil)
		}
	}

	if cert.CheckSignatureFrom(cert) == nil {
		return nil, errors.New(errors.CertificateError, errors.SelfSigned, nil)
	}

	bundle := new(Bundle)
	bundle.Cert = cert
	bundle.Key = key
	bundle.Issuer = &cert.Issuer
	bundle.Subject = &cert.Subject

	bundle.buildHostnames()

	chains, err := cert.Verify(b.VerifyOptions())
	if err != nil {
		log.Debugf("verification failed: %v", err)
		// If the error was an unknown authority, try to fetch
		// the intermediate specified in the AIA and add it to
		// the intermediates bundle.
		switch err := err.(type) {
		case x509.UnknownAuthorityError:
			// Do nothing -- have the default case return out.
		default:
			return nil, errors.New(errors.CertificateError, errors.VerifyFailed, err)
		}

		log.Debugf("searching for intermediates via AIA issuer")
		err = b.fetchIntermediates(certs)
		if err != nil {
			log.Debugf("search failed: %v", err)
			return nil, errors.New(errors.CertificateError, errors.VerifyFailed, err)
		}

		log.Debugf("verifying new chain")
		chains, err = cert.Verify(b.VerifyOptions())
		if err != nil {
			log.Debugf("failed to verify chain: %v", err)
			return nil, errors.New(errors.CertificateError, errors.VerifyFailed, err)
		}
		log.Debugf("verify ok")
	}
	var matchingChains [][]*x509.Certificate
	switch flavor {
	case Optimal:
		matchingChains = optimalChains(chains)
	case Ubiquitous:
		matchingChains = ubiquitousChains(chains)
	default:
		matchingChains = ubiquitousChains(chains)
	}
	// don't include the root in the chain
	bundle.Chain = matchingChains[0][:len(matchingChains[0])-1]

	statusCode := int(errors.Success)
	var messages []string
	// Check if bundle is expiring.
	expiringCerts := checkExpiringCerts(bundle.Chain)
	bundle.Expires = helpers.ExpiryTime(bundle.Chain)
	if len(expiringCerts) > 0 {
		statusCode |= errors.BundleExpiringBit
		messages = append(messages, expirationWarning(expiringCerts))
	}
	// Check if bundle contains SHA2 certs.
	if ubiquity.ChainHashUbiquity(matchingChains[0]) <= ubiquity.SHA2Ubiquity {
		statusCode |= errors.BundleNotUbiquitousBit
		messages = append(messages, sha2Warning)
	}
	// Add root store presence info
	root := matchingChains[0][len(matchingChains[0])-1]
	// Check if there is any platform that doesn't trust the chain.
	untrusted := ubiquity.UntrustedPlatforms(root)
	if len(untrusted) > 0 {
		statusCode |= errors.BundleNotUbiquitousBit
		messages = append(messages, untrustedPlatformsWarning(untrusted))
	}

	bundle.Status = &BundleStatus{ExpiringSKIs: getSKIs(bundle.Chain, expiringCerts), Code: statusCode, Messages: messages, Untrusted: untrusted}

	// Check if bundled one is different from the input.
	diff := false
	if len(bundle.Chain) != len(certs) {
		diff = true
	} else {
		for i, newIntermediate := range bundle.Chain {
			// Use signature to differentiate.
			if !bytes.Equal(certs[i].Signature, newIntermediate.Signature) {
				diff = true
				break
			}
		}
	}

	log.Debugf("bundle complete")
	bundle.Status.IsRebundled = diff
	return bundle, nil
}

// checkExpiringCerts returns indices of certs that are expiring within 30 days.
func checkExpiringCerts(chain []*x509.Certificate) (expiringIntermediates []int) {
	now := time.Now()
	for i, cert := range chain {
		if cert.NotAfter.Sub(now).Hours() < 720 {
			expiringIntermediates = append(expiringIntermediates, i)
		}
	}
	return
}

// getSKIs returns a list of cert subject key id  in the bundle chain with matched indices.
func getSKIs(chain []*x509.Certificate, indices []int) (skis []string) {
	for _, index := range indices {
		ski := fmt.Sprintf("%X", chain[index].SubjectKeyId)
		skis = append(skis, ski)
	}
	return
}

// expirationWarning generates a warning message with expiring certs.
func expirationWarning(expiringIntermediates []int) (ret string) {
	if len(expiringIntermediates) == 0 {
		return
	}

	ret = expiringWarningStub
	if len(expiringIntermediates) > 1 {
		ret = ret + "The expiring certs are"
	} else {
		ret = ret + "The expiring cert is"
	}
	for _, index := range expiringIntermediates {
		ret = ret + " #" + strconv.Itoa(index+1)
	}
	ret = ret + " in the chain."
	return
}

// untrustedPlatformsWarning generates a warning message with untrusted platform names.
func untrustedPlatformsWarning(platforms []string) string {
	if len(platforms) == 0 {
		return ""
	}
	msg := untrustedWarningStub
	for i, platform := range platforms {
		if i > 0 {
			msg += ","
		}
		msg += " " + platform
	}
	msg += "."
	return msg
}

// Optimal chains are the shortest chains, with newest intermediates and most advanced crypto suite being the tie breaker.
func optimalChains(chains [][]*x509.Certificate) [][]*x509.Certificate {
	// Find shortest chains
	chains = ubiquity.Filter(chains, ubiquity.CompareChainLength)
	// Find the chains with longest expiry.
	chains = ubiquity.Filter(chains, ubiquity.CompareChainExpiry)
	// Find the chains with more advanced crypto suite
	chains = ubiquity.Filter(chains, ubiquity.CompareChainCryptoSuite)

	return chains
}

// Ubiquitous chains are the chains with highest platform coverage and break ties with the optimal strategy.
func ubiquitousChains(chains [][]*x509.Certificate) [][]*x509.Certificate {
	// Filter out chains with highest cross platform ubiquity.
	chains = ubiquity.Filter(chains, ubiquity.ComparePlatformUbiquity)
	// Filter shortest chains
	chains = ubiquity.Filter(chains, ubiquity.CompareChainLength)
	// Filter chains with highest signature hash ubiquity.
	chains = ubiquity.Filter(chains, ubiquity.CompareChainHashUbiquity)
	// Filter chains with highest keyAlgo ubiquity.
	chains = ubiquity.Filter(chains, ubiquity.CompareChainKeyAlgoUbiquity)
	// Filter chains with intermediates that last longer.
	chains = ubiquity.Filter(chains, ubiquity.CompareExpiryUbiquity)
	// Use the optimal strategy as final tie breaker.
	return optimalChains(chains)
}
