package saml

import (
	"bytes"
	"encoding/base64"
	"encoding/pem"
	"encoding/xml"
	"io/ioutil"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/goware/saml/xmlsec"
	"github.com/pkg/errors"
)

// Session represents a user session. It is returned by the
// SessionProvider implementation's GetSession method. Fields here
// are used to set fields in the SAML assertion.
type Session struct {
	ID         string
	CreateTime time.Time
	ExpireTime time.Time
	Index      string

	NameID         string
	Groups         []string
	UserID         string
	UserFullname   string
	UserName       string
	UserEmail      string
	UserCommonName string
	UserSurname    string
	UserGivenName  string
}

// IdpAuthnRequest is used by IdentityProvider to handle a single authentication request.
type IdpAuthnRequest struct {
	IDP *IdentityProvider

	// Address set in the SubjectConfirmation element of the Assertion
	Address string

	RelayState              string
	RequestBuffer           []byte
	Request                 AuthnRequest
	ServiceProviderMetadata *Metadata
	ACSEndpoint             *IndexedEndpoint
	Assertion               *Assertion
	AssertionBuffer         []byte
	Response                *Response
}

// IdentityProvider represents an identity provider.
type IdentityProvider struct {

	// Identifier of the IdP entity  (must be a URI)
	EntityID string

	MetadataURL string

	SSOURL string

	SecurityOpts

	// File system location of the private key file
	KeyFile string

	// File system location of the cert file
	CertFile string

	// Private key can also be provided as a param
	// For now we need to write to a temp file since xmlsec requires a physical file to validate the document signature
	PrivkeyPEM string

	// Cert can also be provided as a param
	// For now we need to write to a temp file since xmlsec requires a physical file to validate the document signature
	PubkeyPEM string

	pemCert atomic.Value

	// Service provide settings
	SPMetadataURL string
	SPMetadata    *Metadata

	SPAcsURL string
}

// PrivkeyFile returns a physical path where the IdP's key can be accessed.
func (idp *IdentityProvider) PrivkeyFile() (string, error) {
	if idp.KeyFile != "" {
		return idp.KeyFile, nil
	}
	if idp.PrivkeyPEM != "" {
		return writeFile([]byte(idp.PrivkeyPEM))
	}
	return "", errors.New("missing idp private key")
}

// PubkeyFile returns a physical path where the IdP's public key can be
// accessed.
func (idp *IdentityProvider) PubkeyFile() (string, error) {
	if idp.CertFile != "" {
		return validateKeyFile(idp.CertFile, nil)
	}
	if idp.PubkeyPEM != "" {
		return validateKeyFile(writeFile([]byte(idp.PubkeyPEM)))
	}
	return "", errors.New("missing idp public key")
}

// Cert returns a *pem.Block value that corresponds to the IdP's certificate.
func (idp *IdentityProvider) Cert() (*pem.Block, error) {
	if v := idp.pemCert.Load(); v != nil {
		return v.(*pem.Block), nil
	}
	certFile, err := idp.PubkeyFile()
	if err != nil {
		return nil, err
	}

	fp, err := os.Open(certFile)
	if err != nil {
		return nil, err
	}
	defer fp.Close()

	buf, err := ioutil.ReadAll(fp)
	if err != nil {
		return nil, err
	}

	cert, _ := pem.Decode(buf)
	if cert == nil {
		return nil, errors.New("failed to decode idp cert")
	}

	idp.pemCert.Store(cert)

	return cert, nil
}

// Metadata returns a metadata value based on the IdP's data.
func (idp *IdentityProvider) Metadata() (*Metadata, error) {
	cert, err := idp.Cert()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cert")
	}
	certStr := base64.StdEncoding.EncodeToString(cert.Bytes)

	metadata := &Metadata{
		EntityID:   idp.MetadataURL,
		ValidUntil: Now().Add(defaultValidDuration),
		IDPSSODescriptor: &IDPSSODescriptor{
			ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
			KeyDescriptor: []KeyDescriptor{
				KeyDescriptor{
					Use: "signing",
					KeyInfo: KeyInfo{
						Certificate: certStr,
					},
				},
				KeyDescriptor{
					Use: "encryption",
					KeyInfo: KeyInfo{
						Certificate: certStr,
					},
					EncryptionMethods: []EncryptionMethod{
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#aes128-cbc"},
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#aes192-cbc"},
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#aes256-cbc"},
						EncryptionMethod{Algorithm: "http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p"},
					},
				},
			},
			NameIDFormat: []string{
				"urn:oasis:names:tc:SAML:2.0:nameid-format:transient",
			},
			SingleSignOnService: []Endpoint{
				{
					Binding:  HTTPRedirectBinding,
					Location: idp.SSOURL,
				},
				{
					Binding:  HTTPPostBinding,
					Location: idp.SSOURL,
				},
			},
		},
	}

	return metadata, nil
}

// MakeAssertion produces a SAML assertion for the given request and assigns it
// to req.Assertion.
func (req *IdpAuthnRequest) MakeAssertion(session *Session) error {
	cert, err := req.IDP.Cert()
	if err != nil {
		return err
	}

	signatureTemplate := xmlsec.DefaultSignature(pem.EncodeToMemory(cert))
	attributes := []Attribute{}
	if session.UserName != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "uid",
			Name:         "urn:oid:0.9.2342.19200300.100.1.1",
			NameFormat:   "urn:oasis:names:tc:SAML:2.0:attrname-format:uri",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserName,
			}},
		})
	}

	if session.UserEmail != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "eduPersonPrincipalName",
			Name:         "urn:oid:1.3.6.1.4.1.5923.1.1.1.6",
			NameFormat:   "urn:oasis:names:tc:SAML:2.0:attrname-format:uri",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserEmail,
			}},
		})
	}
	if session.UserSurname != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "sn",
			Name:         "urn:oid:2.5.4.4",
			NameFormat:   "urn:oasis:names:tc:SAML:2.0:attrname-format:uri",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserSurname,
			}},
		})
	}
	if session.UserGivenName != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "givenName",
			Name:         "urn:oid:2.5.4.42",
			NameFormat:   "urn:oasis:names:tc:SAML:2.0:attrname-format:uri",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserGivenName,
			}},
		})
	}

	if session.UserCommonName != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "cn",
			Name:         "urn:oid:2.5.4.3",
			NameFormat:   "urn:oasis:names:tc:SAML:2.0:attrname-format:uri",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserCommonName,
			}},
		})
	}

	if session.UserID != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "MASTUsername",
			Name:         "userid",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserID,
			}},
		})
	}

	if session.UserEmail != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "MASTEmail",
			Name:         "email",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserEmail,
			}},
		})
	}

	if session.UserFullname != "" {
		attributes = append(attributes, Attribute{
			FriendlyName: "MASTName",
			Name:         "fullname",
			Values: []AttributeValue{AttributeValue{
				Type:  "xs:string",
				Value: session.UserFullname,
			}},
		})
	}

	if len(session.Groups) != 0 {
		groupMemberAttributeValues := []AttributeValue{}
		for _, group := range session.Groups {
			groupMemberAttributeValues = append(groupMemberAttributeValues, AttributeValue{
				Type:  "xs:string",
				Value: group,
			})
		}
		attributes = append(attributes, Attribute{
			FriendlyName: "eduPersonAffiliation",
			Name:         "urn:oid:1.3.6.1.4.1.5923.1.1.1.1",
			NameFormat:   "urn:oasis:names:tc:SAML:2.0:attrname-format:uri",
			Values:       groupMemberAttributeValues,
		})
	}

	idpMetadata, err := req.IDP.Metadata()
	if err != nil {
		return err
	}

	spNameQualifier := func() string {
		if meta := req.ServiceProviderMetadata; meta != nil {
			return meta.EntityID
		}
		return ""
	}

	req.Assertion = &Assertion{
		ID:           NewID(),
		IssueInstant: Now(),
		Version:      "2.0",
		Issuer: &Issuer{
			Format: "XXX",
			Value:  idpMetadata.EntityID,
		},
		Signature: &signatureTemplate,
		Subject: &Subject{
			NameID: &NameID{
				Format:          "urn:oasis:names:tc:SAML:2.0:nameid-format:transient",
				NameQualifier:   idpMetadata.EntityID,
				SPNameQualifier: spNameQualifier(),
				Value:           session.NameID,
			},
			SubjectConfirmation: &SubjectConfirmation{
				Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer",
				SubjectConfirmationData: SubjectConfirmationData{
					Address:      req.Address,
					InResponseTo: req.Request.ID,
					NotOnOrAfter: Now().Add(IssueLifetime),
					Recipient: func() string {
						switch {
						case req.ACSEndpoint != nil:
							return req.ACSEndpoint.Location
						case req.ServiceProviderMetadata != nil && req.ServiceProviderMetadata.SPSSODescriptor != nil:
							for _, acs := range req.ServiceProviderMetadata.SPSSODescriptor.AssertionConsumerService {
								if acs.Binding == "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" {
									return acs.Location
								}
							}
						default:
							return req.Request.AssertionConsumerServiceURL
						}
						return ""
					}(),
				},
			},
		},
		Conditions: &Conditions{
			NotBefore:    Now(),
			NotOnOrAfter: Now().Add(IssueLifetime),
			AudienceRestriction: func() *AudienceRestriction {
				if req.ServiceProviderMetadata != nil {
					return &AudienceRestriction{
						Audience: &Audience{Value: req.ServiceProviderMetadata.EntityID},
					}
				}
				return nil
			}(),
		},
		AuthnStatement: &AuthnStatement{
			AuthnInstant: session.CreateTime,
			SessionIndex: session.Index,
			SubjectLocality: SubjectLocality{
				Address: req.Address,
			},
			AuthnContext: AuthnContext{
				AuthnContextClassRef: &AuthnContextClassRef{
					Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport",
				},
			},
		},
		AttributeStatement: &AttributeStatement{
			Attributes: attributes,
		},
	}

	return nil
}

// MarshalAssertion produces a valid and signed XML assertion.
func (req *IdpAuthnRequest) MarshalAssertion() error {
	buf, err := xml.Marshal(req.Assertion)
	if err != nil {
		return err
	}

	keyFile, err := req.IDP.PrivkeyFile()
	if err != nil {
		return err
	}

	buf, err = xmlsec.Sign(buf, keyFile, &xmlsec.ValidationOptions{
		EnableIDAttrHack: true,
	})
	if err != nil {
		if IsSecurityException(err, &req.IDP.SecurityOpts) {
			return err
		}
	}

	req.IDP.SPMetadataURL = (func() string {
		if req.Request.Issuer.Value != "" {
			return req.Request.Issuer.Value
		}
		if req.ServiceProviderMetadata != nil {
			return req.ServiceProviderMetadata.EntityID
		}
		return ""
	})()

	spCertFile, err := req.IDP.GetSPCertFile()
	if err != nil {
		return err
	}

	// EncryptedDataTemplate
	tpl := xmlsec.NewEncryptedDataTemplate(
		"http://www.w3.org/2001/04/xmlenc#aes128-cbc",
		"http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p",
	)

	// TODO: pick an encryption algorithm from the actual metadata.
	buf, err = xmlsec.Encrypt(tpl, buf, spCertFile, "aes-128-cbc")
	if err != nil {
		if IsSecurityException(err, &req.IDP.SecurityOpts) {
			return err
		}
	}

	req.AssertionBuffer = bytes.TrimSpace(bytes.TrimPrefix(buf, []byte(`<?xml version="1.0"?>`)))

	return nil
}

// MakeResponse computes the Response field of the IdpAuthnRequest
func (req *IdpAuthnRequest) MakeResponse() error {
	if req.AssertionBuffer == nil {
		if err := req.MarshalAssertion(); err != nil {
			return err
		}
	}

	req.Response = &Response{
		Destination:  req.Assertion.Subject.SubjectConfirmation.SubjectConfirmationData.Recipient,
		ID:           NewID(),
		InResponseTo: req.Request.ID,
		IssueInstant: Now(),
		Version:      "2.0",
		Issuer: &Issuer{
			Format: "urn:oasis:names:tc:SAML:2.0:nameid-format:entity",
			Value:  req.IDP.MetadataURL,
		},
		Status: &Status{
			StatusCode: StatusCode{
				Value: StatusSuccess,
			},
		},
		EncryptedAssertion: &EncryptedAssertion{
			EncryptedData: req.AssertionBuffer,
		},
	}
	if req.Response.Destination == "" {
		return errors.New("missing response destination")
	}
	return nil
}

// GetSPCertFile returns a physical path where the SP's certificate can be
// accessed.
func (idp *IdentityProvider) GetSPCertFile() (string, error) {
	meta, err := idp.GetSPMetadata()
	if err != nil {
		return "", err
	}

	if meta.SPSSODescriptor == nil {
		return "", errors.New("missing sp sso descriptor")
	}

	cert := ""
	for _, keyDescriptor := range meta.SPSSODescriptor.KeyDescriptor {
		if keyDescriptor.Use == "encryption" {
			cert = keyDescriptor.KeyInfo.Certificate
			break
		}
	}

	if cert == "" {
		for _, keyDescriptor := range meta.SPSSODescriptor.KeyDescriptor {
			if keyDescriptor.KeyInfo.Certificate != "" {
				cert = keyDescriptor.KeyInfo.Certificate
				break
			}
		}
	}

	if cert == "" {
		return "", errors.New("missing sp cert")
	}

	certBytes, _ := base64.StdEncoding.DecodeString(cert)

	certBytes = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})

	return writeFile(certBytes)
}

// GetSPMetadata returns a the SP's metadata value
func (idp *IdentityProvider) GetSPMetadata() (*Metadata, error) {
	if idp.SPMetadata != nil {
		m := *(idp.SPMetadata)
		return &m, nil
	}

	if idp.SPMetadataURL == "" {
		return nil, errors.New("missing sp metadata url")
	}

	res, err := http.Get(idp.SPMetadataURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var metadata Metadata
	err = xml.Unmarshal(buf, &metadata)
	if err != nil {
		return nil, err
	}

	idp.SPMetadata = &metadata
	return &metadata, nil
}
