//go:build !windows
// +build !windows

package sigstore

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hashicorp/go-hclog"
	"github.com/sigstore/cosign/cmd/cosign/cli/fulcio"
	"github.com/sigstore/cosign/pkg/cosign"
	"github.com/sigstore/cosign/pkg/cosign/bundle"
	"github.com/sigstore/cosign/pkg/oci"
	rekor "github.com/sigstore/rekor/pkg/generated/client"
	"github.com/sigstore/sigstore/pkg/signature/payload"
	corev1 "k8s.io/api/core/v1"
)

const (
	// Signature Verification Selector
	signatureVerifiedSelector = "sigstore-validation:passed"
)

type Sigstore interface {
	AttestContainerSignatures(ctx context.Context, status *corev1.ContainerStatus) ([]string, error)
	FetchImageSignatures(ctx context.Context, imageName string) ([]oci.Signature, error)
	SelectorValuesFromSignature(oci.Signature, string) (*SelectorsFromSignatures, error)
	ExtractSelectorsFromSignatures(signatures []oci.Signature, containerID string) []SelectorsFromSignatures
	ShouldSkipImage(imageID string) (bool, error)
	AddSkippedImage(imageID []string)
	ClearSkipList()
	AddAllowedSubject(subject string)
	EnableAllowSubjectList(bool)
	ClearAllowedSubjects()
	SetRekorURL(rekorURL string) error
	SetLogger(logger hclog.Logger)
}

// The following structs are used to go through the payload json objects
type BundleSignature struct {
	Content   string            `json:"content"`
	Format    string            `json:"format"`
	PublicKey map[string]string `json:"publicKey"`
}

type BundleSpec struct {
	Data      map[string]map[string]string `json:"data"`
	Signature BundleSignature              `json:"signature"`
}

type BundleBody struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Spec       BundleSpec `json:"spec"`
}

type SelectorsFromSignatures struct {
	Subject        string
	Content        string
	LogID          string
	IntegratedTime string
}

func New(cache Cache, logger hclog.Logger) Sigstore {
	return &sigstoreImpl{
		functionHooks: sigstoreFunctionHooks{
			// verifyFunction does all the images signatures checks, returning the verified signatures. If there were no valid signatures, it returns an error.
			verifyFunction:             cosign.VerifyImageSignatures,
			fetchImageManifestFunction: remote.Get,
			checkOptsFunction:          defaultCheckOptsFunction,
		},

		rekorURL: url.URL{
			Scheme: rekor.DefaultSchemes[0],
			Host:   rekor.DefaultHost,
			Path:   rekor.DefaultBasePath,
		},
		logger:        logger,
		sigstorecache: cache,
	}
}

func defaultCheckOptsFunction(rekorURL url.URL) (*cosign.CheckOpts, error) {
	switch {
	case rekorURL.Host == "":
		return nil, errors.New("rekor URL host is empty")
	case rekorURL.Scheme == "":
		return nil, errors.New("rekor URL scheme is empty")
	case rekorURL.Path == "":
		return nil, errors.New("rekor URL path is empty")
	}

	rootCerts, err := fulcio.GetRoots()
	if err != nil {
		return nil, fmt.Errorf("failed to get fulcio root certificates: %w", err)
	}

	co := &cosign.CheckOpts{
		// Set the rekor client
		RekorClient: rekor.NewHTTPClientWithConfig(nil, rekor.DefaultTransportConfig().WithBasePath(rekorURL.Path).WithHost(rekorURL.Host)),
		RootCerts:   rootCerts,
	}
	co.IntermediateCerts, err = fulcio.GetIntermediates()

	return co, err
}

type sigstoreImpl struct {
	functionHooks    sigstoreFunctionHooks
	skippedImages    map[string]struct{}
	allowListEnabled bool
	subjectAllowList map[string]struct{}
	rekorURL         url.URL
	logger           hclog.Logger
	sigstorecache    Cache
}

func (s *sigstoreImpl) SetLogger(logger hclog.Logger) {
	s.logger = logger
}

// FetchImageSignatures retrieves signatures for specified image via cosign, using the specified rekor server.
// Returns a list of verified signatures, and an error if any.
func (s *sigstoreImpl) FetchImageSignatures(ctx context.Context, imageName string) ([]oci.Signature, error) {
	ref, err := name.ParseReference(imageName)
	if err != nil {
		return nil, fmt.Errorf("error parsing image reference: %w", err)
	}

	if _, err := s.ValidateImage(ref); err != nil {
		return nil, fmt.Errorf("could not validate image reference digest: %w", err)
	}

	co, err := s.functionHooks.checkOptsFunction(s.rekorURL)
	if err != nil {
		return nil, fmt.Errorf("could not create cosign check options: %w", err)
	}
	sigs, ok, err := s.functionHooks.verifyFunction(ctx, ref, co)
	switch {
	case err != nil:
		return nil, fmt.Errorf("error verifying signature: %w", err)
	case !ok:
		return nil, fmt.Errorf("bundle not verified for %q", imageName)
	default:
		return sigs, nil
	}
}

// ExtractSelectorsFromSignatures extracts selectors from a list of image signatures.
// returns a list of selector strings.
func (s *sigstoreImpl) ExtractSelectorsFromSignatures(signatures []oci.Signature, containerID string) []SelectorsFromSignatures {
	// Payload can be empty if the attestor fails to retrieve it
	if signatures == nil {
		return nil
	}
	var selectors []SelectorsFromSignatures
	for _, sig := range signatures {
		// verify which subject
		sigSelectors, err := s.SelectorValuesFromSignature(sig, containerID)
		if err != nil {
			s.logger.Error("error extracting selectors from signature", "error", err)
		}
		if sigSelectors != nil {
			selectors = append(selectors, *sigSelectors)
		}
	}
	return selectors
}

// SelectorValuesFromSignature extracts selectors from a signature.
// returns a list of selectors.
func (s *sigstoreImpl) SelectorValuesFromSignature(signature oci.Signature, containerID string) (*SelectorsFromSignatures, error) {
	subject, err := getSignatureSubject(signature)
	if err != nil {
		return nil, fmt.Errorf("error getting signature subject: %w", err)
	}

	if subject == "" {
		return nil, fmt.Errorf("error getting signature subject: %w", errors.New("empty subject"))
	}

	if s.allowListEnabled {
		if _, ok := s.subjectAllowList[subject]; !ok {
			return nil, fmt.Errorf("subject %q not in allow-list", subject)
		}
	}

	selectorsFromSignatures := &SelectorsFromSignatures{Subject: subject}

	bundle, err := signature.Bundle()
	if err != nil {
		return nil, fmt.Errorf("error getting signature bundle: %w", err)
	}
	sigContent, err := getBundleSignatureContent(bundle)
	if err != nil {
		return nil, fmt.Errorf("error getting signature content: %w", err)
	}
	selectorsFromSignatures.Content = sigContent

	if bundle.Payload.LogID == "" {
		return nil, fmt.Errorf("error getting signature log ID: %w", errors.New("empty log ID"))
	}
	selectorsFromSignatures.LogID = bundle.Payload.LogID

	if bundle.Payload.IntegratedTime == 0 {
		return nil, fmt.Errorf("error getting signature integrated time: %w", errors.New("integrated time is 0"))
	}
	selectorsFromSignatures.IntegratedTime = strconv.FormatInt(bundle.Payload.IntegratedTime, 10)
	return selectorsFromSignatures, nil
}

// ShouldSkipImage checks the skip list for the image ID in the container status.
// If the image ID is found in the skip list, it returns true.
// If the image ID is not found in the skip list, it returns false.
func (s *sigstoreImpl) ShouldSkipImage(imageID string) (bool, error) {
	if len(s.skippedImages) == 0 {
		return false, nil
	}
	if imageID == "" {
		return false, errors.New("image ID is empty")
	}
	_, ok := s.skippedImages[imageID]
	return ok, nil
}

// AddSkippedImage adds the image ID and selectors to the skip list.
func (s *sigstoreImpl) AddSkippedImage(imageIDList []string) {
	if s.skippedImages == nil {
		s.skippedImages = make(map[string]struct{})
	}
	for _, imageID := range imageIDList {
		s.skippedImages[imageID] = struct{}{}
	}
}

// ClearSkipList clears the skip list.
func (s *sigstoreImpl) ClearSkipList() {
	s.skippedImages = nil
}

// ValidateImage validates if the image manifest hash matches the digest in the image reference
func (s *sigstoreImpl) ValidateImage(ref name.Reference) (bool, error) {
	dgst, ok := ref.(name.Digest)
	if !ok {
		return false, fmt.Errorf("reference %T is not a digest", ref)
	}
	desc, err := s.functionHooks.fetchImageManifestFunction(dgst)
	if err != nil {
		return false, err
	}
	if len(desc.Manifest) == 0 {
		return false, errors.New("manifest is empty")
	}
	hash, _, err := v1.SHA256(bytes.NewReader(desc.Manifest))
	if err != nil {
		return false, err
	}

	return validateRefDigest(dgst, hash.String())
}

func (s *sigstoreImpl) AddAllowedSubject(subject string) {
	if s.subjectAllowList == nil {
		s.subjectAllowList = make(map[string]struct{})
	}
	s.subjectAllowList[subject] = struct{}{}
}

func (s *sigstoreImpl) ClearAllowedSubjects() {
	s.subjectAllowList = nil
}

func (s *sigstoreImpl) EnableAllowSubjectList(flag bool) {
	s.allowListEnabled = flag
}

func (s *sigstoreImpl) AttestContainerSignatures(ctx context.Context, status *corev1.ContainerStatus) ([]string, error) {
	skip, err := s.ShouldSkipImage(status.ImageID)
	if err != nil {
		return nil, fmt.Errorf("failed attesting container signature: %w", err)
	}
	if skip {
		return []string{signatureVerifiedSelector}, nil
	}

	imageID := status.ImageID

	cachedSignature := s.sigstorecache.GetSignature(imageID)
	if cachedSignature != nil {
		s.logger.Debug("Found cached signature", "image_id", imageID)
	} else {
		signatures, err := s.FetchImageSignatures(ctx, imageID)
		if err != nil {
			return nil, err
		}

		selectors := s.ExtractSelectorsFromSignatures(signatures, status.ContainerID)

		cachedSignature = &Item{
			Key:   imageID,
			Value: selectors,
		}

		s.logger.Debug("Caching signature", "image_id", imageID)
		s.sigstorecache.PutSignature(*cachedSignature)
	}

	var selectorsString []string
	if len(cachedSignature.Value) > 0 {
		for _, selector := range cachedSignature.Value {
			toString := selectorsToString(selector, status.ContainerID)
			selectorsString = append(selectorsString, toString...)
		}
		selectorsString = append(selectorsString, signatureVerifiedSelector)
	}

	return selectorsString, nil
}

func (s *sigstoreImpl) SetRekorURL(rekorURL string) error {
	if rekorURL == "" {
		return errors.New("rekor URL is empty")
	}
	rekorURI, err := url.Parse(rekorURL)
	if err != nil {
		return fmt.Errorf("failed parsing rekor URI: %w", err)
	}
	if rekorURI.Scheme != "" && rekorURI.Scheme != "https" {
		return fmt.Errorf("invalid rekor URL Scheme %q", rekorURI.Scheme)
	}
	if rekorURI.Host == "" {
		return fmt.Errorf("host is required on rekor URL")
	}
	s.rekorURL = *rekorURI
	return nil
}

func getSignatureSubject(signature oci.Signature) (string, error) {
	if signature == nil {
		return "", errors.New("signature is nil")
	}
	ss := payload.SimpleContainerImage{}
	pl, err := signature.Payload()
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(pl, &ss); err != nil {
		return "", err
	}
	cert, err := signature.Cert()
	if err != nil {
		return "", fmt.Errorf("failed to access signature certificate: %w", err)
	}

	if cert != nil {
		return certSubject(cert), nil
	}
	if len(ss.Optional) > 0 {
		if subjString, ok := ss.Optional["subject"]; ok {
			if subj, ok := subjString.(string); ok {
				return subj, nil
			}
		}
	}

	return "", errors.New("no subject found in signature")
}

func getBundleSignatureContent(bundle *bundle.RekorBundle) (string, error) {
	if bundle == nil {
		return "", errors.New("bundle is nil")
	}
	body64, ok := bundle.Payload.Body.(string)
	if !ok {
		returnedType := fmt.Sprintf("expected payload body to be a string but got %T instead", bundle.Payload.Body)
		return "", errors.New(returnedType)
	}
	body, err := base64.StdEncoding.DecodeString(body64)
	if err != nil {
		return "", err
	}
	var bundleBody BundleBody
	if err := json.Unmarshal(body, &bundleBody); err != nil {
		return "", fmt.Errorf("failed to parse bundle body: %w", err)
	}

	if bundleBody.Spec.Signature.Content == "" {
		return "", errors.New("bundle payload body has no signature content")
	}

	return bundleBody.Spec.Signature.Content, nil
}

func selectorsToString(selectors SelectorsFromSignatures, containerID string) []string {
	var selectorsString []string
	if selectors.Subject != "" {
		selectorsString = append(selectorsString, fmt.Sprintf("%s:image-signature-subject:%s", containerID, selectors.Subject))
	}
	if selectors.Content != "" {
		selectorsString = append(selectorsString, fmt.Sprintf("%s:image-signature-content:%s", containerID, selectors.Content))
	}
	if selectors.LogID != "" {
		selectorsString = append(selectorsString, fmt.Sprintf("%s:image-signature-logid:%s", containerID, selectors.LogID))
	}
	if selectors.IntegratedTime != "" {
		selectorsString = append(selectorsString, fmt.Sprintf("%s:image-signature-integrated-time:%s", containerID, selectors.IntegratedTime))
	}
	return selectorsString
}

func certSubject(c *x509.Certificate) string {
	switch {
	case c == nil:
		return ""
	case len(c.EmailAddresses) > 0:
		return c.EmailAddresses[0]
	case len(c.URIs) > 0:
		// removing leading '//' from c.URIs[0].String()
		return strings.TrimPrefix(c.URIs[0].String(), "//")
	default:
		return ""
	}
}

func validateRefDigest(dgst name.Digest, digest string) (bool, error) {
	if dgst.DigestStr() == digest {
		return true, nil
	}
	return false, fmt.Errorf("digest %s does not match %s", digest, dgst.DigestStr())
}

type verifyFunctionType func(context.Context, name.Reference, *cosign.CheckOpts) ([]oci.Signature, bool, error)

type fetchImageManifestFunctionType func(name.Reference, ...remote.Option) (*remote.Descriptor, error)

type checkOptsFunctionType func(url.URL) (*cosign.CheckOpts, error)

type sigstoreFunctionHooks struct {
	verifyFunction             verifyFunctionType
	fetchImageManifestFunction fetchImageManifestFunctionType
	checkOptsFunction          checkOptsFunctionType
}
