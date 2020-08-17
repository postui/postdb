package postdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/postui/postdb/q"
	bolt "go.etcd.io/bbolt"
)

// Tx represents a transaction on the database.
type Tx struct {
	ns []byte
	tx *bolt.Tx
}

// List returns some posts
func (tx *Tx) List(qs ...q.Query) (posts []q.Post) {
	metaBucket := tx.bucket(postmetaKey)
	indexBucket := tx.bucket(postindexKey)
	idIndexBucket := indexBucket.Bucket(postidKey)

	var cur *bolt.Cursor
	var prefixs [][]byte
	var filter func(*q.Post) bool
	var n uint32

	var res q.Resolver
	for _, q := range qs {
		q.Resolve(&res)
	}

	queryOwner := len(res.Owner) > 0
	orderASC := res.Order >= q.ASC

	var offsetPkey []byte
	if len(res.Offset) > 0 {
		offsetPkey = idIndexBucket.Get([]byte(res.Offset))
		if offsetPkey == nil {
			return
		}
	}

	if len(res.IDs) > 0 {
		posts = make([]q.Post, len(res.IDs))
		for i, id := range res.IDs {
			pkey := idIndexBucket.Get([]byte(id))
			if pkey != nil {
				v := metaBucket.Get(pkey)
				if v != nil {
					post, err := q.PostFromBytes(v)
					if err == nil && bytes.Equal(post.PKey[:], pkey) && (len(res.Tags) == 0 || containsSlice(post.Tags, res.Tags)) && (!queryOwner || post.Owner == res.Owner) {
						if len(res.Keys) > 0 {
							tx.readKV(post, res.Keys)
						}
						if res.Filter(*post.Clone()) {
							posts[i] = *post
							n++
							if res.Limit > 0 && n >= res.Limit {
								break
							}
						}
					}
				}
			}
		}
		posts = posts[:n]
	} else if len(res.Tags) > 0 {
		cur = indexBucket.Bucket(posttagKey).Cursor()
		prefixs = make([][]byte, len(res.Tags))
		var i int
		for _, tag := range res.Tags {
			p := make([]byte, len(tag)+1)
			copy(p, []byte(tag))
			prefixs[i] = p
			i++
		}
		if queryOwner {
			filter = func(post *q.Post) bool {
				return post.Owner == res.Owner
			}
		}
	} else if queryOwner {
		prefix := make([]byte, len(res.Owner)+1)
		copy(prefix, []byte(res.Owner))
		cur = indexBucket.Bucket(postownerKey).Cursor()
		prefixs = [][]byte{prefix}
	} else {
		c := metaBucket.Cursor()
		var k, v []byte
		if offsetPkey != nil {
			k, v = c.Seek(offsetPkey)
		} else {
			if orderASC {
				k, v = c.First()
			} else {
				k, v = c.Last()
			}
		}
		for {
			if k == nil {
				break
			}
			post, err := q.PostFromBytes(v)
			if err == nil && bytes.Equal(post.PKey[:], k) {
				if len(res.Keys) > 0 {
					tx.readKV(post, res.Keys)
				}
				if res.Filter(*post.Clone()) {
					posts = append(posts, *post)
					n++
					if res.Limit > 0 && n >= res.Limit {
						break
					}
				}
			}
			if orderASC {
				k, v = c.Next()
			} else {
				k, v = c.Prev()
			}
		}
	}

	if cur != nil {
		pl := len(prefixs)
		a := make([][]*q.Post, pl)
		for i, prefix := range prefixs {
			var k []byte
			if offsetPkey != nil {
				p := make([]byte, len(prefix)+12)
				copy(p, prefix)
				copy(p[len(prefix):], offsetPkey)
				k, _ = cur.Seek(p)
			} else {
				k, _ = cur.Seek(prefix)
			}

			ok := func(k []byte) bool {
				return len(k) == len(prefix)+12 && bytes.HasPrefix(k, prefix)
			}
			if !ok(k) {
				break
			}

			// move cursor to the last key firstly when order by DESC
			if !orderASC && offsetPkey == nil {
				for {
					k, _ = cur.Next()
					if k == nil {
						k, _ = cur.Last()
						break
					} else if !ok(k) {
						k, _ = cur.Prev()
						break
					}
				}
			}

			for {
				if !ok(k) {
					break
				}
				pkey := k[len(k)-12:]
				if pkey != nil {
					if i == 0 {
						data := metaBucket.Get(pkey)
						if data != nil {
							post, err := q.PostFromBytes(data)
							if err == nil && bytes.Equal(post.PKey[:], pkey) && (filter == nil || filter(post) == true) {
								if len(res.Keys) > 0 {
									tx.readKV(post, res.Keys)
								}
								if res.Filter(*post.Clone()) {
									if pl == 1 {
										posts = append(posts, *post)
									} else {
										a[0] = append(a[0], post)
									}
									n++
									if res.Limit > 0 && n >= res.Limit {
										break
									}
								}
							}
						}
					} else if past := a[i-1]; len(past) > 0 {
						for _, p := range past {
							if bytes.Equal(p.PKey[:], pkey) {
								if i == pl-1 {
									posts = append(posts, *p)
								} else {
									a[i] = append(a[i], p)
								}
							}
						}
					}
				}
				if orderASC {
					k, _ = cur.Next()
				} else {
					k, _ = cur.Prev()
				}
			}
		}
	}
	return
}

func (tx *Tx) readKV(post *q.Post, keys []string) {
	postkvBucket := tx.bucket(postkvKey).Bucket([]byte(post.ID))
	if postkvBucket == nil {
		return
	}

	var rest []string
	for _, key := range keys {
		kl := len(key)
		if kl > 0 {
			if strings.HasSuffix(key, "*") {
				c := postkvBucket.Cursor()
				// key equals "*"
				if kl == 1 {
					for k, v := c.First(); k != nil; k, v = c.Next() {
						post.KV[string(k)] = v
					}
					return
				}
				prefix := []byte(key[:kl-1])
				for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
					post.KV[string(k)] = v
				}
			} else if _, ok := post.KV[key]; !ok {
				rest = append(rest, key)
			}
		}
	}
	for _, key := range rest {
		v := postkvBucket.Get([]byte(key))
		if v != nil {
			post.KV[key] = v
		}
	}
}

// Get returns the post
func (tx *Tx) Get(qs ...q.Query) (*q.Post, error) {
	var res q.Resolver
	for _, q := range qs {
		q.Resolve(&res)
	}

	var metaBytes []byte
	if len(res.IDs) > 0 {
		idIndexBucket := tx.bucket(postindexKey).Bucket(postidKey)
		pkey := idIndexBucket.Get([]byte(res.IDs[0]))
		if pkey != nil {
			metaBytes = tx.bucket(postmetaKey).Get(pkey)
		}
	}
	if metaBytes == nil {
		return nil, ErrNotFound
	}

	post, err := q.PostFromBytes(metaBytes)
	if err != nil {
		return nil, err
	}

	if len(res.Keys) > 0 {
		tx.readKV(post, res.Keys)
	}

	return post, nil
}

// Put puts a new post
func (tx *Tx) Put(qs ...q.Query) (*q.Post, error) {
	indexBucket := tx.bucket(postindexKey)
	idIndexBucket := indexBucket.Bucket(postidKey)

RE:
	post := q.NewPost()
	for _, q := range qs {
		q.Apply(post)
	}
	// ensure the post.ID is unique
	if idIndexBucket.Get([]byte(post.ID)) != nil {
		log.Printf("[warn] duplicate id %s", post.ID)
		goto RE
	}

	err := tx.PutPost(post)
	if err != nil {
		return nil, err
	}

	return post, nil
}

// PutPost puts a new post
func (tx *Tx) PutPost(post *q.Post) (err error) {
	metaBucket := tx.bucket(postmetaKey)
	indexBucket := tx.bucket(postindexKey)
	kvBucket := tx.bucket(postkvKey)
	idIndexBucket := indexBucket.Bucket(postidKey)

	if metaBucket.Get(post.PKey[:]) != nil {
		return fmt.Errorf("duplicate pkey %v", post.PKey)
	}
	err = metaBucket.Put(post.PKey[:], post.MetaBytes())
	if err != nil {
		return
	}

	if idIndexBucket.Get([]byte(post.ID)) != nil {
		return fmt.Errorf("duplicate id %s", post.ID)
	}
	err = idIndexBucket.Put([]byte(post.ID), post.PKey[:])
	if err != nil {
		return
	}

	if len(post.Alias) > 0 {
		if idIndexBucket.Get([]byte(post.Alias)) != nil {
			return ErrDuplicateAlias
		}
		err = idIndexBucket.Put([]byte(post.Alias), post.PKey[:])
		if err != nil {
			return
		}
	}

	if len(post.Owner) > 0 {
		ownerIndexBucket := indexBucket.Bucket(postownerKey)
		keypath := join([]byte(post.Owner), post.PKey[:], 0)
		err = ownerIndexBucket.Put(keypath, []byte{1})
		if err != nil {
			return
		}
	}

	if len(post.Tags) > 0 {
		tagIndexBucket := indexBucket.Bucket(posttagKey)
		for _, tag := range post.Tags {
			keypath := join([]byte(tag), post.PKey[:], 0)
			err = tagIndexBucket.Put(keypath, []byte{1})
			if err != nil {
				return
			}
		}
	}

	postkvBucket, err := kvBucket.CreateBucketIfNotExists([]byte(post.ID))
	if err != nil {
		return
	}

	if len(post.KV) > 0 {
		for k, v := range post.KV {
			err = postkvBucket.Put([]byte(k), v)
			if err != nil {
				return
			}
		}
	}

	return
}

// Update updates the post
func (tx *Tx) Update(qs ...q.Query) error {
	metaBucket := tx.bucket(postmetaKey)
	indexBucket := tx.bucket(postindexKey)
	kvBucket := tx.bucket(postkvKey)
	idIndexBucket := indexBucket.Bucket(postidKey)

	post, err := tx.Get(qs...)
	if err != nil {
		return err
	}

	copy := post.Clone()
	for _, q := range qs {
		q.Apply(copy)
	}

	shouldUpdateMeta := copy.Status != post.Status

	// update alias index
	if copy.Alias != post.Alias {
		if idIndexBucket.Get([]byte(copy.Alias)) != nil {
			return ErrDuplicateAlias
		}
		if len(post.Alias) > 0 {
			err = idIndexBucket.Delete([]byte(post.Alias))
			if err != nil {
				return err
			}
		}
		if len(copy.Alias) > 0 {
			err = idIndexBucket.Put([]byte(copy.Alias), copy.PKey[:])
			if err != nil {
				return err
			}
		}
		if !shouldUpdateMeta {
			shouldUpdateMeta = true
		}
	}

	// update owner index
	if copy.Owner != post.Owner {
		ownerIndexBucket := indexBucket.Bucket(postownerKey)
		if len(post.Owner) > 0 {
			keypath := join([]byte(post.Owner), post.PKey[:], 0)
			err = ownerIndexBucket.Delete(keypath)
			if err != nil {
				return err
			}
		}
		if len(copy.Owner) > 0 {
			keypath := join([]byte(copy.Owner), copy.PKey[:], 0)
			err = ownerIndexBucket.Put(keypath, []byte{1})
			if err != nil {
				return err
			}
		}
		if !shouldUpdateMeta {
			shouldUpdateMeta = true
		}
	}

	// update tags index
	if strings.Join(copy.Tags, "") != strings.Join(post.Tags, "") {
		tagIndexBucket := indexBucket.Bucket(posttagKey)
		if len(post.Tags) > 0 {
			for _, tag := range post.Tags {
				keypath := join([]byte(tag), post.PKey[:], 0)
				err = tagIndexBucket.Delete(keypath)
				if err != nil {
					return err
				}
			}
		}
		if len(copy.Tags) > 0 {
			for _, tag := range copy.Tags {
				keypath := join([]byte(tag), copy.PKey[:], 0)
				err = tagIndexBucket.Put(keypath, []byte{1})
				if err != nil {
					return err
				}
			}
		}
		if !shouldUpdateMeta {
			shouldUpdateMeta = true
		}
	}

	if len(copy.KV) > 0 {
		postkvBucket := kvBucket.Bucket([]byte(copy.ID))
		for k, v := range copy.KV {
			err = postkvBucket.Put([]byte(k), v)
			if err != nil {
				return err
			}
		}
		if !shouldUpdateMeta {
			shouldUpdateMeta = true
		}
	}

	if shouldUpdateMeta {
		copy.Modtime = uint32(time.Now().Unix())
		err = metaBucket.Put(copy.PKey[:], copy.MetaBytes())
		if err != nil {
			return err
		}
	}

	return nil
}

// MoveTo moves the post
func (tx *Tx) MoveTo(qs ...q.Query) error {
	post, err := tx.Get(qs...)
	if err != nil {
		return err
	}

	var res q.Resolver
	for _, q := range qs {
		q.Resolve(&res)
	}

	if res.Anchor == "" {
		return errors.New("missing anchor id")
	}

	anchorPost, err := tx.Get(q.ID(res.Anchor))
	if err != nil {
		return err
	}

	post.PKey = anchorPost.PKey
	// todo: implement moveTo function

	return nil
}

// DeleteKV deletes the post kv
func (tx *Tx) DeleteKV(qs ...q.Query) error {
	post, err := tx.Get(qs...)
	if err != nil {
		return err
	}

	var res q.Resolver
	for _, q := range qs {
		q.Resolve(&res)
	}

	if len(res.Keys) > 0 {
		kvBucket := tx.bucket(postkvKey)
		postkvBucket := kvBucket.Bucket([]byte(post.ID))
		if postkvBucket != nil {
			var rest []string
			for _, key := range res.Keys {
				kl := len(key)
				if kl > 0 {
					if strings.HasSuffix(key, "*") {
						c := postkvBucket.Cursor()
						// key equals "*"
						if kl == 1 {
							for k, _ := c.First(); k != nil; k, _ = c.Next() {
								err := postkvBucket.Delete(k)
								if err != nil {
									return err
								}
							}
							return nil
						}
						prefix := []byte(key[:kl-1])
						for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
							err := postkvBucket.Delete(k)
							if err != nil {
								return err
							}
						}
					} else if _, ok := post.KV[key]; !ok {
						rest = append(rest, key)
					}
				}
			}
			for _, key := range rest {
				err := postkvBucket.Delete([]byte(key))
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Delete deletes the post
func (tx *Tx) Delete(qs ...q.Query) (n int, err error) {
	if len(qs) == 0 {
		return 0, nil
	}

	metaBucket := tx.bucket(postmetaKey)
	indexBucket := tx.bucket(postindexKey)
	kvBucket := tx.bucket(postkvKey)
	idIndexBucket := indexBucket.Bucket(postidKey)
	posts := tx.List(qs...)

	for _, post := range posts {
		err = metaBucket.Delete(post.PKey[:])
		if err != nil {
			return
		}

		err = idIndexBucket.Delete([]byte(post.ID))
		if err != nil {
			return
		}

		if len(post.Alias) > 0 {
			err = idIndexBucket.Delete([]byte(post.Alias))
			if err != nil {
				return
			}
		}

		if len(post.Owner) > 0 {
			ownerIndexBucket := indexBucket.Bucket(postownerKey)
			keypath := join([]byte(post.Owner), []byte(post.ID), 0)
			err = ownerIndexBucket.Delete(keypath)
			if err != nil {
				return
			}
		}

		if len(post.Tags) > 0 {
			tagIndexBucket := indexBucket.Bucket(posttagKey)
			for _, tag := range post.Tags {
				keypath := join([]byte(tag), []byte(post.ID), 0)
				err = tagIndexBucket.Delete(keypath)
				if err != nil {
					return
				}
			}
		}

		err = kvBucket.DeleteBucket([]byte(post.ID))
		if err != nil {
			return
		}
	}

	n = len(posts)
	return
}

func (tx *Tx) bucket(name []byte) *bolt.Bucket {
	if len(tx.ns) > 0 {
		return tx.tx.Bucket(join(tx.ns, name, 0))
	}
	return tx.tx.Bucket(name)
}

// Rollback closes the transaction and ignores all previous updates. Read-only
// transactions must be rolled back and not committed.
func (tx *Tx) Rollback() error {
	return tx.tx.Rollback()
}

// Commit writes all changes to disk and updates the meta page.
// Returns an error if a disk write error occurs, or if Commit is
// called on a read-only transaction.
func (tx *Tx) Commit() error {
	return tx.tx.Commit()
}

// WriteTo writes the entire database to a writer.
// If err == nil then exactly tx.Size() bytes will be written into the writer.
func (tx *Tx) WriteTo(w io.Writer) (int64, error) {
	return tx.tx.WriteTo(w)
}
