/*
Package darc in most of our projects we need some kind of access control to protect resources. Instead of having a simple password
or public key for authentication, we want to have access control that can be:
evolved with a threshold number of keys
be delegated
So instead of having a fixed list of identities that are allowed to access a resource, the goal is to have an evolving
description of who is allowed or not to access a certain resource.
*/
package darc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/dedis/cothority"
	"github.com/dedis/cothority/ocs/darc/expression"
	"github.com/dedis/kyber"
	"github.com/dedis/kyber/sign/schnorr"
	"github.com/dedis/kyber/util/key"
	"github.com/dedis/protobuf"
)

const evolve = "_evolve"

// InitRules initialise a set of rules with only the default action.
// TODO we need to support multi-signature sign-offs, i.e. we initialise a
// custom expression where multiple signatures are required to evolve the darc.
func InitRules(owners []*Identity) Rules {
	ids := make([]string, len(owners))
	for i, o := range owners {
		ids[i] = o.String()
	}
	rs := make(Rules)
	rs[evolve] = expression.InitOrExpr(ids...)
	return rs
}

// NewDarc initialises a darc-structure given its owners and users. Note that
// the BaseID is empty if the Version is 0, it must be computed using
// GetBaseID.
func NewDarc(rules Rules, desc []byte) *Darc {
	return &Darc{
		Version:     0,
		Description: desc,
		Signature:   nil,
		Rules:       rules,
	}
}

// Copy all the fields of a Darc except the signature
func (d *Darc) Copy() *Darc {
	dCopy := &Darc{
		Version:     d.Version,
		Description: copyBytes(d.Description),
		BaseID:      copyBytes(d.BaseID),
	}
	newRules := make(Rules)
	for k, v := range d.Rules {
		newRules[k] = v
	}
	dCopy.Rules = newRules
	return dCopy
}

// Equal returns true if both darcs point to the same data.
func (d *Darc) Equal(d2 *Darc) bool {
	return d.GetID().Equal(d2.GetID())
}

// ToProto returns a protobuf representation of the Darc-structure.
// We copy a darc first to keep only invariant fields which exclude
// the delegation signature.
func (d *Darc) ToProto() ([]byte, error) {
	dc := d.Copy()
	b, err := protobuf.Encode(dc)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// NewDarcFromProto interprets a protobuf-representation of the darc and
// returns a created Darc.
func NewDarcFromProto(protoDarc []byte) *Darc {
	d := &Darc{}
	protobuf.Decode(protoDarc, d)
	return d
}

// GetID returns the Darc ID, which is a digest of the values in the Darc.
// The digest does not include the signature.
func (d Darc) GetID() ID {
	h := sha256.New()
	verBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(verBytes, d.Version)
	h.Write(verBytes)
	h.Write(d.Description)
	h.Write(d.BaseID)

	actions := make([]string, len(d.Rules))
	var i int
	for k := range d.Rules {
		actions[i] = string(k)
		i++
	}
	sort.Strings(actions)
	for _, a := range actions {
		h.Write([]byte(a))
		h.Write(d.Rules[Action(a)])
	}
	return h.Sum(nil)
}

// GetIdentityString returns the string representation of the ID.
func (d Darc) GetIdentityString() string {
	return NewIdentityDarc(d.GetID()).String()
}

// GetBaseID returns the base ID or the ID of this darc if its the
// first darc.
func (d Darc) GetBaseID() ID {
	if d.Version == 0 {
		return d.GetID()
	}
	return d.BaseID
}

// AddRule adds a new action expression-pair, the action must not exist.
func (r Rules) AddRule(a Action, expr expression.Expr) error {
	if _, ok := r[a]; ok {
		return errors.New("action already exists")
	}
	r[a] = expr
	return nil
}

// UpdateRule updates an existing action-expression pair, it cannot be the
// evolve action.
func (r Rules) UpdateRule(a Action, expr expression.Expr) error {
	if isEvolution(a) {
		return errors.New("cannot update evolution")
	}
	return r.updateRule(a, expr)
}

// DeleteRules deletes an action, it cannot delete the evolve action.
func (r Rules) DeleteRules(a Action) error {
	if isEvolution(a) {
		return errors.New("cannot delete evolution")
	}
	if _, ok := r[a]; !ok {
		return errors.New("action does not exist")
	}
	delete(r, a)
	return nil
}

// Contains checks if the action a is in the rules.
func (r Rules) Contains(a Action) bool {
	_, ok := r[a]
	return ok
}

// GetEvolutionExpr returns the expression that describes the evolution action.
func (r Rules) GetEvolutionExpr() expression.Expr {
	return r[evolve]
}

// UpdateEvolution will update the "evolve" action, which allows identities
// that satisfies the expression to evolve the Darc. Take extreme care when
// using this function.
func (r Rules) UpdateEvolution(expr expression.Expr) error {
	return r.updateRule(evolve, expr)
}

func (r Rules) updateRule(a Action, expr expression.Expr) error {
	if _, ok := r[a]; !ok {
		return errors.New("action does not exist")
	}
	r[a] = expr
	return nil
}

func isEvolution(action Action) bool {
	if action == evolve {
		return true
	}
	return false
}

// Evolve evolves a darc, the latest valid darc needs to sign the new darc.
// Only if one of the previous owners signs off on the new darc will it be
// valid and accepted to sign on behalf of the old darc. The path can be nil
// unless if the previousOwner is an SignerEd25519 and found directly in the
// previous darc.
func (d *Darc) Evolve(path []*Darc, prevOwner *Signer) error {
	d.Signature = nil
	prevDarc := path[len(path)-1]
	d.Version = prevDarc.Version + 1
	if len(path) == 0 {
		return errors.New("path should not be empty")
	}
	d.BaseID = prevDarc.GetBaseID()
	sig, err := NewDarcSignature(prevOwner, d.GetID(), path)
	if err != nil {
		return errors.New("error creating a darc signature for evolution: " + err.Error())
	}
	if sig == nil {
		return errors.New("the resulting signature is nil")
	}
	d.Signature = sig
	return nil
}

// EvolveOnline works like Evolve, but doesn't inlcude all the necessary data
// to verify the update in an offline setting. This is enough for the use case
// where the ocs stores all darcs in its internal database.  The service
// verifying the signature will have to verify if there is a valid path from
// the previous darc to the signer.
func (d *Darc) EvolveOnline(prevd *Darc, prevOwner *Signer) error {
	/*
		d.Signature = nil
		d.Version = prevd.Version + 1
		if prevd.BaseID == nil {
			id := prevd.GetID()
			d.BaseID = &id
		}
		path := &SignaturePath{Signer: *prevOwner.Identity()}
		sig, err := NewDarcSignature(d.GetID(), path, prevOwner)
		if err != nil {
			return errors.New("error creating a darc signature for evolution: " + err.Error())
		}
		if sig != nil {
			d.Signature = sig
		} else {
			return errors.New("the resulting signature is nil")
		}
	*/
	return nil
}

// IncrementVersion updates the version number of the Darc
func (d *Darc) IncrementVersion() {
	d.Version++
}

// Verify will check that the darc is correct, an error is returned if
// something is wrong.
func (d *Darc) Verify() error {
	return d.VerifyWithCB(func(s string) *Darc {
		return nil
	})
}

// VerifyWithCB will check that the darc is correct, an error is returned if
// something is wrong.  The caller should supply the callback getDarc because
// if one of the IDs in the expression is a Darc ID, then this function needs a
// way to retrieve the correct Darc according to that ID. Note that getDarc is
// responsible to return the newest Darc.
func (d *Darc) VerifyWithCB(getDarc func(string) *Darc) error {
	if d == nil {
		return errors.New("darc is nil")
	}
	if d.Version == 0 {
		return nil // nothing to verify on the genesis Darc
	}

	if d.Signature == nil || len(d.Signature.Path) == 0 {
		return errors.New("signature missing")
	}

	var prev *Darc
	for i, dPath := range d.Signature.Path {
		if prev == nil && dPath.Version == 0 {
			prev = dPath
			continue
		}
		if err := verifyOneEvolution(dPath, prev, getDarc); err != nil {
			return fmt.Errorf("verification failed on index %d with error: %v", i, err)
		}
		prev = dPath
	}

	signer, err := d.GetSignerDarc()
	if err != nil {
		return err
	}
	return verifyOneEvolution(d, signer, getDarc)
}

// GetSignerDarc returns the darc that signed this darc, which is the last
// element in the signature path.
func (d Darc) GetSignerDarc() (*Darc, error) {
	if d.Signature == nil {
		// signature is nil if there are no evolution - nothing to sign
		return nil, nil
	}
	if len(d.Signature.Path) == 0 {
		return nil, errors.New("signature but no darcs")
	}
	n := len(d.Signature.Path)
	prev := (d.Signature.Path)[n-1]
	if prev.Version+1 != d.Version {
		return nil, errors.New("not clean evolution - version mismatch")
	}
	return prev, nil
}

// CheckRequest checks the given request and returns an error if it cannot be
// accepted.
func (d Darc) CheckRequest(r *Request) error {
	if !d.GetID().Equal(r.ID) {
		return fmt.Errorf("darc id mismatch")
	}
	if !d.Rules.Contains(r.Action) {
		return fmt.Errorf("%v does not exist", r.Action)
	}
	digest, err := r.Hash()
	if err != nil {
		return err
	}
	for i, id := range r.Identities {
		if err := id.Verify(digest, r.Signatures[i]); err != nil {
			return err
		}
	}
	validIDs := r.GetIdentityStrings()
	ok, err := expression.DefaultParser(d.Rules[r.Action], validIDs...)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("expression '%s' for the keys '%v' evaluated to false",
			d.Rules[r.Action], validIDs)
	}
	return nil
}

func (d Darc) String() string {
	s := fmt.Sprintf("ID:\t%x\nBase:\t%x\nVer:\t%d\nRules:", d.GetID(), d.GetBaseID(), d.Version)
	for k, v := range d.Rules {
		s += fmt.Sprintf("\n\t%s - \"%s\"", k, v)
	}
	sigStr := "<nil>"
	if d.Signature != nil {
		sigStr = fmt.Sprintf("%x", d.Signature.Signature)
	}
	s += fmt.Sprintf("\nSignature: %s", sigStr)
	signer, err := d.GetSignerDarc()
	var signerStr string
	if err != nil {
		signerStr = "<" + err.Error() + ">"
	} else if signer == nil {
		signerStr = "<nil>"
	} else {
		signerStr = fmt.Sprintf("%x", signer.GetID())
	}
	s += fmt.Sprintf("\nSignerDarc: %s", signerStr)
	return s
}

// IsNull returns true if this DarcID is not initialised.
func (di ID) IsNull() bool {
	return di == nil
}

// Equal compares with another DarcID.
func (di ID) Equal(other ID) bool {
	return bytes.Equal([]byte(di), []byte(other))
}

// NewDarcSignature creates a new darc signature by hashing (baseID + pathMsg),
// where PathMsg is retrieved from a given signature path, and signing it
// with a given signer.
func NewDarcSignature(signer *Signer, id ID, path []*Darc) (*Signature, error) {
	if signer == nil {
		return nil, errors.New("signer missing")
	}
	if len(id) == 0 {
		return nil, errors.New("id missing")
	}
	if len(path) == 0 {
		return nil, errors.New("path missing")
	}

	hash, err := hashAll(id, darcsMsg(path))
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(hash)
	if err != nil {
		return nil, err
	}
	return &Signature{Signature: sig, Signer: *signer.Identity(), Path: path}, nil
}

// GetPathMsg returns the concatenated Darc-IDs of the path.
func (s *Signature) GetPathMsg() []byte {
	return darcsMsg(s.Path)
}

// PathContains checks whether the path contains the ID.
func (s *Signature) PathContains(cb func(*Darc) bool) bool {
	for _, d := range s.Path {
		if cb(d) {
			return true
		}
	}
	return false
}

// Verify returns nil if the signature is correct, or an error
// if something is wrong.
func (s *Signature) verify(msg []byte, base ID) error {
	if base == nil {
		return errors.New("base-darc is missing")
	}
	if len(s.Path) == 0 {
		return errors.New("no path stored in signaturepath")
	}
	sigBase := (s.Path)[0].GetID()
	if !sigBase.Equal(base) {
		return errors.New("Base-darc is not at root of path")
	}
	hash, err := hashAll(msg, s.GetPathMsg())
	if err != nil {
		return err
	}
	return s.Signer.Verify(hash, s.Signature)
}

func hashAll(msgs ...[]byte) ([]byte, error) {
	h := sha256.New()
	for _, msg := range msgs {
		if _, err := h.Write(msg); err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func darcsMsg(darcs []*Darc) []byte {
	if len(darcs) == 0 {
		return []byte{}
	}
	var path []byte
	for _, darc := range darcs {
		path = append(path, darc.GetID()...)
	}
	return path
}

// verifyOneEvolution verifies that one evolution is performed correctly. That
// is, there should exist a signature in the newDarc that is signed by one of
// the identities with the evolve permission in the oldDarc. The message is the
// signature path specified in the newDarc, its ID and the base ID of the darc.
// TODO we need to support multi-signature sign-offs.
func verifyOneEvolution(newDarc, prevDarc *Darc, getDarc func(string) *Darc) error {
	// check base ID
	if newDarc.BaseID == nil {
		return errors.New("nil base ID")
	}
	if !newDarc.GetBaseID().Equal(prevDarc.GetBaseID()) {
		return errors.New("base IDs are not equal")
	}

	// check version
	if newDarc.Version != prevDarc.Version+1 {
		return fmt.Errorf("incorrect version, new version should be %d but it is %d",
			prevDarc.Version+1, newDarc.Version)
	}

	// check that signer has the permission
	signer := newDarc.Signature.Signer.String()
	if err := checkEvolutionPermission(prevDarc.Rules.GetEvolutionExpr(), getDarc, signer); err != nil {
		return err
	}

	// perform the verification
	return newDarc.Signature.verify(newDarc.GetID(), prevDarc.GetBaseID())
}

// checkEvolutionPermission checks whether the expression evaluates to true
// given a list of identities.
func checkEvolutionPermission(expr expression.Expr, getDarc func(string) *Darc, ids ...string) error {
	Y := expression.InitParser(func(s string) bool {
		if strings.HasPrefix(s, "darc") {
			// getDarc is responsible for returning the latest Darc
			// but the path should contain the darc ID s.
			d := getDarc(s)
			if d.Verify() != nil {
				return false
			}
			if !d.Signature.PathContains(func(d *Darc) bool {
				return d.GetIdentityString() == s
			}) {
				return false
			}

			// Try to evaluate the latest darc, it might not
			// evaluate to true because the signer could be an
			// older darc.
			ok, err := expression.DefaultParser(d.Rules.GetEvolutionExpr(), ids...)
			if err == nil && ok {
				return true
			}

			// If the signer is not on the latest darc, then we
			// look for older darcs in the Path of the latest darc.
			// The searching mechanism starts from the base Darc,
			// so we skip all the Darcs before the ID (s) that we
			// are trying to evaluate.
			var pathSearchStart bool
			ok = d.Signature.PathContains(func(d *Darc) bool {
				if d.GetIdentityString() == s {
					pathSearchStart = true
				}
				if !pathSearchStart {
					return false
				}
				ok, err := expression.DefaultParser(d.Rules.GetEvolutionExpr(), ids...)
				if err != nil {
					return false
				}
				return ok
			})
			return ok
		}
		for _, id := range ids {
			if id == s {
				return true
			}
		}
		return false
	})
	res, err := expression.ParseExpr(Y, expr)
	if err != nil {
		return fmt.Errorf("evaluation failed on '%s' with error: %v", expr, err)
	}
	if res != true {
		return fmt.Errorf("expression '%s' evaluated to false", expr)
	}
	return nil
}

// Type returns an integer representing the type of key held in the signer.
// It is compatible with Identity.Type. For an empty signer, -1 is returned.
func (s *Signer) Type() int {
	switch {
	case s.Ed25519 != nil:
		return 1
	case s.X509EC != nil:
		return 2
	default:
		return -1
	}
}

// Identity returns an identity struct with the pre initialised fields
// for the appropriate signer.
func (s *Signer) Identity() *Identity {
	switch s.Type() {
	case 1:
		return &Identity{Ed25519: &IdentityEd25519{Point: s.Ed25519.Point}}
	case 2:
		return &Identity{X509EC: &IdentityX509EC{Public: s.X509EC.Point}}
	default:
		return nil
	}
}

// Sign returns a signature in bytes for a given messages by the signer
func (s *Signer) Sign(msg []byte) ([]byte, error) {
	if msg == nil {
		return nil, errors.New("nothing to sign, message is empty")
	}
	switch s.Type() {
	case 0:
		return nil, errors.New("cannot sign with a darc")
	case 1:
		return s.Ed25519.Sign(msg)
	case 2:
		return s.X509EC.Sign(msg)
	default:
		return nil, errors.New("unknown signer type")
	}
}

// GetPrivate returns the private key, if one exists.
func (s *Signer) GetPrivate() (kyber.Scalar, error) {
	switch s.Type() {
	case 1:
		return s.Ed25519.Secret, nil
	case 0, 2:
		return nil, errors.New("signer lacks a private key")
	default:
		return nil, errors.New("signer is of unknown type")
	}
}

// Equal first checks the type of the two identities, and if they match,
// it returns if their data is the same.
func (id *Identity) Equal(id2 *Identity) bool {
	if id.Type() != id2.Type() {
		return false
	}
	switch id.Type() {
	case 0:
		return id.Darc.Equal(id2.Darc)
	case 1:
		return id.Ed25519.Equal(id2.Ed25519)
	case 2:
		return id.X509EC.Equal(id2.X509EC)
	}
	return false
}

// Type returns an int indicating what type of identity this is. If all
// identities are nil, it returns -1.
func (id *Identity) Type() int {
	switch {
	case id.Darc != nil:
		return 0
	case id.Ed25519 != nil:
		return 1
	case id.X509EC != nil:
		return 2
	}
	return -1
}

// TypeString returns the string of the type of the identity.
func (id *Identity) TypeString() string {
	switch id.Type() {
	case 0:
		return "darc"
	case 1:
		return "ed25519"
	case 2:
		return "x509ec"
	default:
		return "No identity"
	}
}

// String returns the string representation of the identity
func (id *Identity) String() string {
	switch id.Type() {
	case 0:
		return fmt.Sprintf("%s:%x", id.TypeString(), id.Darc.ID)
	case 1:
		return fmt.Sprintf("%s:%s", id.TypeString(), id.Ed25519.Point.String())
	case 2:
		return fmt.Sprintf("%s:%x", id.TypeString(), id.X509EC.Public)
	default:
		return "No identity"
	}
}

// Verify returns nil if the signature is correct, or an error if something
// went wrong.
func (id *Identity) Verify(msg, sig []byte) error {
	switch id.Type() {
	case 0:
		return errors.New("cannot verify a darc-signature")
	case 1:
		return id.Ed25519.Verify(msg, sig)
	case 2:
		return id.X509EC.Verify(msg, sig)
	default:
		return errors.New("unknown identity")
	}
}

// NewIdentityDarc creates a new darc identity struct given a darcid
func NewIdentityDarc(id ID) *Identity {
	return &Identity{
		Darc: &IdentityDarc{
			ID: id,
		},
	}
}

// Equal returns true if both IdentityDarcs point to the same data.
func (idd *IdentityDarc) Equal(idd2 *IdentityDarc) bool {
	return bytes.Compare(idd.ID, idd2.ID) == 0
}

// NewIdentityEd25519 creates a new Ed25519 identity struct given a point
func NewIdentityEd25519(point kyber.Point) *Identity {
	return &Identity{
		Ed25519: &IdentityEd25519{
			Point: point,
		},
	}
}

// Equal returns true if both IdentityEd25519 point to the same data.
func (ide *IdentityEd25519) Equal(ide2 *IdentityEd25519) bool {
	return ide.Point.Equal(ide2.Point)
}

// Verify returns nil if the signature is correct, or an error if something
// fails.
func (ide *IdentityEd25519) Verify(msg, sig []byte) error {
	return schnorr.Verify(cothority.Suite, ide.Point, msg, sig)
}

// NewIdentityX509EC creates a new X509EC identity struct given a point
func NewIdentityX509EC(public []byte) *Identity {
	return &Identity{
		X509EC: &IdentityX509EC{
			Public: public,
		},
	}
}

// Equal returns true if both IdentityX509EC point to the same data.
func (idkc *IdentityX509EC) Equal(idkc2 *IdentityX509EC) bool {
	return bytes.Compare(idkc.Public, idkc2.Public) == 0
}

type sigRS struct {
	R *big.Int
	S *big.Int
}

// Verify returns nil if the signature is correct, or an error if something
// fails.
func (idkc *IdentityX509EC) Verify(msg, s []byte) error {
	public, err := x509.ParsePKIXPublicKey(idkc.Public)
	if err != nil {
		return err
	}
	hash := sha512.Sum384(msg)
	sig := &sigRS{}
	_, err = asn1.Unmarshal(s, sig)
	if err != nil {
		return err
	}
	if ecdsa.Verify(public.(*ecdsa.PublicKey), hash[:], sig.R, sig.S) {
		return nil
	}
	return errors.New("Wrong signature")
}

// NewSignerEd25519 initializes a new SignerEd25519 given a public and private keys.
// If any of the given values is nil or both are nil, then a new key pair is generated.
// It returns a signer.
func NewSignerEd25519(point kyber.Point, secret kyber.Scalar) *Signer {
	if point == nil || secret == nil {
		kp := key.NewKeyPair(cothority.Suite)
		point, secret = kp.Public, kp.Private
	}
	return &Signer{Ed25519: &SignerEd25519{
		Point:  point,
		Secret: secret,
	}}
}

// Sign creates a schnorr signautre on the message
func (eds *SignerEd25519) Sign(msg []byte) ([]byte, error) {
	return schnorr.Sign(cothority.Suite, eds.Secret, msg)
}

// Hash computes the digest of the request, the identities and signatures are
// not included.
func (r *Request) Hash() ([]byte, error) {
	h := sha256.New()
	if _, err := h.Write(r.ID); err != nil {
		return nil, err
	}
	if _, err := h.Write([]byte(r.Action)); err != nil {
		return nil, err
	}
	if _, err := h.Write(r.Msg); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// GetIdentityStrings returns a slice of identity strings, this is useful for
// creating a parser.
func (r *Request) GetIdentityStrings() []string {
	res := make([]string, len(r.Identities))
	for i, id := range r.Identities {
		res[i] = id.String()
	}
	return res
}

// NewRequest creates a new request which can be verified by a Darc.
func NewRequest(darcID ID, action Action, msg []byte, signers ...*Signer) (*Request, error) {
	r := Request{
		ID:     darcID,
		Action: action,
		Msg:    msg,
	}

	digest, err := r.Hash()
	if err != nil {
		return nil, err
	}

	r.Signatures = make([][]byte, len(signers))
	r.Identities = make([]*Identity, len(signers))
	for i, signer := range signers {
		r.Identities[i] = signer.Identity()
		r.Signatures[i], err = signer.Sign(digest)
		if err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// NewSignerX509EC creates a new SignerX509EC - mostly for tests
func NewSignerX509EC() *Signer {
	return nil
}

// Sign creates a RSA signature on the message
func (kcs *SignerX509EC) Sign(msg []byte) ([]byte, error) {
	return nil, errors.New("not yet implemented")
}

func copyBytes(a []byte) []byte {
	b := make([]byte, len(a))
	copy(b, a)
	return b
}
