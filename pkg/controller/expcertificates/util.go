/*
Copyright 2020 The Jetstack cert-manager contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certificates

import (
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/workqueue"

	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha2"
	cmlisters "github.com/jetstack/cert-manager/pkg/client/listers/certmanager/v1alpha2"
	controllerpkg "github.com/jetstack/cert-manager/pkg/controller"
	"github.com/jetstack/cert-manager/pkg/util"
	"github.com/jetstack/cert-manager/pkg/util/pki"
)

type GetFunc func(namespace, name string) (interface{}, error)

func CertificateGetFunc(lister cmlisters.CertificateLister) GetFunc {
	return func(namespace, name string) (interface{}, error) {
		return lister.Certificates(namespace).Get(name)
	}
}

// EnqueueCertificatesForSecretNameFunc will enqueue Certificate resources that
// satisfy a CertificatePredicateFunc based upon the name of the Secret resource
// being processed.
// This is used to trigger Certificates to reconcile for changes to the Secret
// being managed.
func EnqueueCertificatesForSecretNameFunc(log logr.Logger, lister cmlisters.CertificateLister, selector labels.Selector,
	secretNamePredicate WithCertificatePredicateFunc, queue workqueue.Interface) func(obj interface{}) {
	return func(obj interface{}) {
		s, ok := obj.(*corev1.Secret)
		if !ok {
			log.Info("Non-Secret type resource passed to EnqueueCertificatesForSecretFunc")
			return
		}

		certs, err := ListCertificatesMatchingPredicate(lister.Certificates(s.Namespace), selector, secretNamePredicate(s.Name))
		if err != nil {
			log.Error(err, "Failed listing Certificate resources")
			return
		}

		for _, cert := range certs {
			key, err := controllerpkg.KeyFunc(cert)
			if err != nil {
				log.Error(err, "Error determining 'key' for resource")
				continue
			}
			queue.Add(key)
		}
	}
}

type WithCertificatePredicateFunc func(string) CertificatePredicateFunc

type CertificatePredicateFunc func(*cmapi.Certificate) bool

func WithSecretNamePredicateFunc(name string) CertificatePredicateFunc {
	return func(crt *cmapi.Certificate) bool {
		return crt.Spec.SecretName == name
	}
}

func WithNextPrivateKeySecretNamePredicateFunc(name string) CertificatePredicateFunc {
	return func(crt *cmapi.Certificate) bool {
		if crt.Status.NextPrivateKeySecretName == nil {
			return false
		}
		return *crt.Status.NextPrivateKeySecretName == name
	}
}

func ListCertificatesMatchingPredicate(lister cmlisters.CertificateNamespaceLister, selector labels.Selector, predicate CertificatePredicateFunc) ([]*cmapi.Certificate, error) {
	crts, err := lister.List(selector)
	if err != nil {
		return nil, err
	}
	out := make([]*cmapi.Certificate, 0)
	for _, crt := range crts {
		if predicate(crt) {
			out = append(out, crt)
		}
	}
	return out, nil
}

type CertificateRequestPredicateFunc func(*cmapi.CertificateRequest) bool

func WithCertificateRevisionPredicateFunc(revision int) CertificateRequestPredicateFunc {
	return func(req *cmapi.CertificateRequest) bool {
		if req.Annotations == nil {
			return false
		}
		return req.Annotations[cmapi.CertificateRequestRevisionAnnotationKey] == fmt.Sprintf("%d", revision)
	}
}

func WithCertificateRequestOwnerPredicateFunc(owner metav1.Object) CertificateRequestPredicateFunc {
	return func(req *cmapi.CertificateRequest) bool {
		return metav1.IsControlledBy(req, owner)
	}
}

func ListCertificateRequestsMatchingPredicates(lister cmlisters.CertificateRequestNamespaceLister, selector labels.Selector, predicates ...CertificateRequestPredicateFunc) ([]*cmapi.CertificateRequest, error) {
	reqs, err := lister.List(selector)
	if err != nil {
		return nil, err
	}
	out := make([]*cmapi.CertificateRequest, 0)
	for _, req := range reqs {
		matches := true
		for _, predicate := range predicates {
			if !predicate(req) {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, req)
		}
	}

	return out, nil
}

// RequestMatchesSpec compares a CertificateRequest with a CertificateSpec
// and returns a list of field names on the Certificate that do not match their
// counterpart fields on the CertificateRequest.
// If decoding the x509 certificate request fails, an error will be returned.
func RequestMatchesSpec(req *cmapi.CertificateRequest, spec cmapi.CertificateSpec) ([]string, error) {
	x509req, err := pki.DecodeX509CertificateRequestBytes(req.Spec.CSRPEM)
	if err != nil {
		return nil, err
	}

	// It is safe to mutate top-level fields in `spec` as it is not a pointer
	// meaning changes will not effect the caller.
	if spec.Subject == nil {
		spec.Subject = &cmapi.X509Subject{}
	}

	var violations []string
	if x509req.Subject.CommonName != spec.CommonName {
		violations = append(violations, "spec.commonName")
	}
	if !util.EqualUnsorted(x509req.DNSNames, spec.DNSNames) {
		violations = append(violations, "spec.dnsNames")
	}
	if !util.EqualUnsorted(pki.IPAddressesToString(x509req.IPAddresses), spec.IPAddresses) {
		violations = append(violations, "spec.ipAddresses")
	}
	if !util.EqualUnsorted(pki.URLsToString(x509req.URIs), spec.URISANs) {
		violations = append(violations, "spec.uriSANs")
	}
	if x509req.Subject.SerialNumber != spec.Subject.SerialNumber {
		violations = append(violations, "spec.subject.serialNumber")
	}
	if !util.EqualUnsorted(x509req.Subject.Organization, spec.Organization) {
		violations = append(violations, "spec.subject.organizations")
	}
	if !util.EqualUnsorted(x509req.Subject.Country, spec.Subject.Countries) {
		violations = append(violations, "spec.subject.countries")
	}
	if !util.EqualUnsorted(x509req.Subject.Locality, spec.Subject.Localities) {
		violations = append(violations, "spec.subject.localities")
	}
	if !util.EqualUnsorted(x509req.Subject.OrganizationalUnit, spec.Subject.OrganizationalUnits) {
		violations = append(violations, "spec.subject.organizationalUnits")
	}
	if !util.EqualUnsorted(x509req.Subject.PostalCode, spec.Subject.PostalCodes) {
		violations = append(violations, "spec.subject.postCodes")
	}
	if !util.EqualUnsorted(x509req.Subject.Province, spec.Subject.Provinces) {
		violations = append(violations, "spec.subject.postCodes")
	}
	if !util.EqualUnsorted(x509req.Subject.StreetAddress, spec.Subject.StreetAddresses) {
		violations = append(violations, "spec.subject.streetAddresses")
	}
	if req.Spec.IsCA != spec.IsCA {
		violations = append(violations, "spec.isCA")
	}
	if !util.EqualKeyUsagesUnsorted(req.Spec.Usages, spec.Usages) {
		violations = append(violations, "spec.usages")
	}
	if spec.Duration != nil && req.Spec.Duration != nil &&
		spec.Duration.Duration != req.Spec.Duration.Duration {
		violations = append(violations, "spec.duration")
	}
	if !reflect.DeepEqual(spec.IssuerRef, req.Spec.IssuerRef) {
		violations = append(violations, "spec.issuerRef")
	}

	return violations, nil
}

// SecretDataAltNamesMatchSpec will compare a Secret resource containing certificate
// data to a CertificateSpec and return a list of 'violations' for any fields that
// do not match their counterparts.
// This is a purposely less comprehensive check than RequestMatchesSpec as some
// issuers override/force certain fields.
func SecretDataAltNamesMatchSpec(secret *corev1.Secret, spec cmapi.CertificateSpec) ([]string, error) {
	x509cert, err := pki.DecodeX509CertificateBytes(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return nil, err
	}

	var violations []string
	if x509cert.Subject.CommonName != spec.CommonName {
		violations = append(violations, "spec.commonName")
	}
	if !util.EqualUnsorted(x509cert.DNSNames, spec.DNSNames) {
		violations = append(violations, "spec.dnsNames")
	}
	if !util.EqualUnsorted(pki.IPAddressesToString(x509cert.IPAddresses), spec.IPAddresses) {
		violations = append(violations, "spec.ipAddresses")
	}
	if !util.EqualUnsorted(pki.URLsToString(x509cert.URIs), spec.URISANs) {
		violations = append(violations, "spec.uriSANs")
	}
	return violations, nil
}