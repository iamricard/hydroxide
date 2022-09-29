package carddav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav/carddav"

	"github.com/emersion/hydroxide/log"
	"github.com/emersion/hydroxide/protonmail"
)

// TODO: use a HTTP error
var errNotFound = errors.New("carddav: not found")

var (
	cleartextCardProps = []string{vcard.FieldVersion, vcard.FieldProductID, "X-PM-LABEL", "X-PM-GROUP"}
	signedCardProps    = []string{vcard.FieldVersion, vcard.FieldProductID, vcard.FieldFormattedName, vcard.FieldUID, vcard.FieldEmail}
)

func formatCard(card vcard.Card, privateKey *openpgp.Entity) (*protonmail.ContactImport, error) {
	vcard.ToV4(card)

	// Add groups to emails
	i := 1
	for _, email := range card[vcard.FieldEmail] {
		if email.Group == "" {
			email.Group = "item" + strconv.Itoa(i)
			i++
		}
	}

	toEncrypt := card
	toSign := make(vcard.Card)
	for _, k := range signedCardProps {
		if fields, ok := toEncrypt[k]; ok {
			toSign[k] = fields
			if k != vcard.FieldVersion {
				delete(toEncrypt, k)
			}
		}
	}

	var contactImport protonmail.ContactImport
	var b bytes.Buffer

	if len(toSign) > 0 {
		if err := vcard.NewEncoder(&b).Encode(toSign); err != nil {
			return nil, err
		}
		signed, err := protonmail.NewSignedContactCard(&b, privateKey)
		if err != nil {
			return nil, err
		}
		contactImport.Cards = append(contactImport.Cards, signed)
		b.Reset()
	}

	if len(toEncrypt) > 0 {
		if err := vcard.NewEncoder(&b).Encode(toEncrypt); err != nil {
			return nil, err
		}
		to := []*openpgp.Entity{privateKey}
		encrypted, err := protonmail.NewEncryptedContactCard(&b, to, privateKey)
		if err != nil {
			return nil, err
		}
		contactImport.Cards = append(contactImport.Cards, encrypted)
		b.Reset()
	}

	return &contactImport, nil
}

func parseAddressObjectPath(p string) (string, error) {
	dirname, filename := path.Split(p)
	ext := path.Ext(filename)
	if dirname != "/contacts/default/" || ext != ".vcf" {
		return "", errNotFound
	}
	return strings.TrimSuffix(filename, ext), nil
}

func formatAddressObjectPath(id string) string {
	return "/contacts/default/" + id + ".vcf"
}

func (b *backend) toAddressObject(contact *protonmail.Contact, req *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	// TODO: handle req

	card := make(vcard.Card)
	for _, c := range contact.Cards {
		md, err := c.Read(b.privateKeys)
		if err != nil {
			return nil, err
		}

		decoded, err := vcard.NewDecoder(md.UnverifiedBody).Decode()
		if err != nil {
			return nil, err
		}

		// The signature can be checked only if md.UnverifiedBody is consumed until
		// EOF
		io.Copy(ioutil.Discard, md.UnverifiedBody)
		if err := md.SignatureError; err != nil {
			return nil, err
		}

		for k, fields := range decoded {
			for _, f := range fields {
				card.Add(k, f)
			}
		}
	}

	return &carddav.AddressObject{
		Path:    formatAddressObjectPath(contact.ID),
		ModTime: contact.ModifyTime.Time(),
		// TODO: stronger ETag
		ETag: fmt.Sprintf("%x%x", contact.ModifyTime, contact.Size),
		Card: card,
	}, nil
}

type backend struct {
	c           *protonmail.Client
	cache       map[string]*protonmail.Contact
	locker      sync.Mutex
	total       int
	privateKeys openpgp.EntityList
}

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return "/", nil
}

func (b *backend) AddressbookHomeSetPath(ctx context.Context) (string, error) {
	return "/contacts", nil
}

func (b *backend) AddressBook(ctx context.Context) (*carddav.AddressBook, error) {
	return &carddav.AddressBook{
		Path:            "/contacts/default",
		Name:            "ProtonMail",
		Description:     "ProtonMail contacts",
		MaxResourceSize: 100 * 1024,
	}, nil
}

func (b *backend) cacheComplete() bool {
	b.locker.Lock()
	defer b.locker.Unlock()
	return b.total >= 0 && len(b.cache) == b.total
}

func (b *backend) getCache(id string) (*protonmail.Contact, bool) {
	b.locker.Lock()
	contact, ok := b.cache[id]
	b.locker.Unlock()
	return contact, ok
}

func (b *backend) putCache(contact *protonmail.Contact) {
	b.locker.Lock()
	b.cache[contact.ID] = contact
	b.locker.Unlock()
}

func (b *backend) deleteCache(id string) {
	b.locker.Lock()
	delete(b.cache, id)
	b.locker.Unlock()
}

func (b *backend) GetAddressObject(ctx context.Context, path string, req *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	id, err := parseAddressObjectPath(path)
	if err != nil {
		return nil, fmt.Errorf("GetAddressObject: %w", err)
	}

	contact, ok := b.getCache(id)
	if !ok {
		if b.cacheComplete() {
			return nil, fmt.Errorf("GetAddressObject: %w", errNotFound)
		}

		if contact, err = b.c.GetContact(id); err != nil {
			return nil, fmt.Errorf("GetAddressObject: %w", err)
		}
		b.putCache(contact)
	}

	o, err := b.toAddressObject(contact, req)
	if err != nil {
		return nil, fmt.Errorf("GetAddressObject: %w", err)
	}
	return o, nil
}

func (b *backend) ListAddressObjects(ctx context.Context, req *carddav.AddressDataRequest) ([]carddav.AddressObject, error) {
	if b.cacheComplete() {
		b.locker.Lock()
		defer b.locker.Unlock()

		aos := make([]carddav.AddressObject, 0, len(b.cache))
		for _, contact := range b.cache {
			ao, err := b.toAddressObject(contact, req)
			if err != nil {
				return nil, fmt.Errorf("ListAddressObjects: %w", err)
			}
			aos = append(aos, *ao)
		}

		return aos, nil
	}

	// Get a list of all contacts
	// TODO: paging support
	total, contacts, err := b.c.ListContacts(0, 0)
	if err != nil {
		return nil, fmt.Errorf("ListAddressObjects: %w", err)
	}
	b.locker.Lock()
	b.total = total
	b.locker.Unlock()

	m := make(map[string]*protonmail.Contact, total)
	for _, contact := range contacts {
		m[contact.ID] = contact
	}

	// Get all contacts cards
	aos := make([]carddav.AddressObject, 0, total)
	page := 0
	for {
		_, contacts, err := b.c.ListContactsExport(page, 0)
		if err != nil {
			return nil, fmt.Errorf("ListAddressObjects: %w", err)
		}

		for _, contactExport := range contacts {
			contact, ok := m[contactExport.ID]
			if !ok {
				continue
			}
			contact.Cards = contactExport.Cards
			b.putCache(contact)

			ao, err := b.toAddressObject(contact, req)
			if err != nil {
				return nil, fmt.Errorf("ListAddressObjects: %w", err)
			}
			aos = append(aos, *ao)
		}

		if len(aos) >= total || len(contacts) == 0 {
			break
		}
		page++
	}

	return aos, nil
}

func (b *backend) QueryAddressObjects(ctx context.Context, query *carddav.AddressBookQuery) ([]carddav.AddressObject, error) {
	req := carddav.AddressDataRequest{AllProp: true}
	if query != nil {
		req = query.DataRequest
	}

	// TODO: optimize
	all, err := b.ListAddressObjects(ctx, &req)
	if err != nil {
		return nil, fmt.Errorf("QueryAddressObjects: %w", err)
	}

	o, err := carddav.Filter(query, all)
	if err != nil {
		return nil, fmt.Errorf("QueryAddressObjects: %w", err)
	}
	return o, nil
}

func (b *backend) PutAddressObject(ctx context.Context, path string, card vcard.Card, opts *carddav.PutAddressObjectOptions) (loc string, err error) {
	id, err := parseAddressObjectPath(path)
	if err != nil {
		return "", fmt.Errorf("PutAddressObject: %w", err)
	}

	contactImport, err := formatCard(card, b.privateKeys[0])
	if err != nil {
		return "", fmt.Errorf("PutAddressObject: %w", err)
	}

	var contact *protonmail.Contact

	var req carddav.AddressDataRequest
	if _, getErr := b.GetAddressObject(ctx, path, &req); getErr == nil {
		contact, err = b.c.UpdateContact(id, contactImport)
		if err != nil {
			return "", fmt.Errorf("PutAddressObject: %w", err)
		}
	} else {
		resps, err := b.c.CreateContacts([]*protonmail.ContactImport{contactImport})
		if err != nil {
			return "", fmt.Errorf("PutAddressObject: %w", err)
		}
		if len(resps) != 1 {
			return "", fmt.Errorf("PutAddressObject: hydroxide/carddav: expected exactly one response when creating contact")
		}
		resp := resps[0]
		if err := resp.Err(); err != nil {
			return "", fmt.Errorf("PutAddressObject: %w", err)
		}
		contact = resp.Response.Contact
	}
	contact.Cards = contactImport.Cards // Not returned by the server

	// TODO: increment b.total if necessary
	b.putCache(contact)
	return formatAddressObjectPath(contact.ID), nil
}

func (b *backend) DeleteAddressObject(ctx context.Context, path string) error {
	id, err := parseAddressObjectPath(path)
	if err != nil {
		return fmt.Errorf("DeleteAddressObject: %w", err)
	}
	resps, err := b.c.DeleteContacts([]string{id})
	if err != nil {
		return fmt.Errorf("DeleteAddressObject: %w", err)
	}
	if len(resps) != 1 {
		return fmt.Errorf("DeleteAddressObject: hydroxide/carddav: expected exactly one response when deleting contact")
	}
	resp := resps[0]
	// TODO: decrement b.total if necessary
	b.deleteCache(id)
	return fmt.Errorf("DeleteAddressObject: %w", resp.Err())
}

func (b *backend) receiveEvents(events <-chan *protonmail.Event) {
	for event := range events {
		b.locker.Lock()
		if event.Refresh&protonmail.EventRefreshContacts != 0 {
			b.cache = make(map[string]*protonmail.Contact)
			b.total = -1
		} else if len(event.Contacts) > 0 {
			for _, eventContact := range event.Contacts {
				switch eventContact.Action {
				case protonmail.EventCreate:
					if b.total >= 0 {
						b.total++
					}
					fallthrough
				case protonmail.EventUpdate:
					b.cache[eventContact.ID] = eventContact.Contact
				case protonmail.EventDelete:
					delete(b.cache, eventContact.ID)
					if b.total >= 0 {
						b.total--
					}
				}
			}
		}
		b.locker.Unlock()
	}
}

func NewHandler(c *protonmail.Client, privateKeys openpgp.EntityList, events <-chan *protonmail.Event) http.Handler {
	if len(privateKeys) == 0 {
		panic("hydroxide/carddav: no private key available")
	}

	b := &backend{
		c:           c,
		cache:       make(map[string]*protonmail.Contact),
		total:       -1,
		privateKeys: privateKeys,
	}

	if events != nil {
		go b.receiveEvents(events)
	}

	handler := &carddav.Handler{Backend: b}
	instrumented := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		logger := log.Http(writer)

		handler.ServeHTTP(logger, request)

		logger.Log().
			Str("method", request.Method).
			Str("URI", request.RequestURI).
			Dur("duration", time.Since(start)).
			Msg("CardDAV backend")
	})
	return instrumented
}
