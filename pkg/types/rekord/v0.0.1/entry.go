/*
Copyright © 2020 Bob Callaway <bcallawa@redhat.com>

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
package rekord

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/sigstore/rekor/pkg/log"
	"github.com/sigstore/rekor/pkg/types"
	"github.com/sigstore/rekor/pkg/util"

	"github.com/asaskevich/govalidator"

	"github.com/go-openapi/strfmt"

	"github.com/sigstore/rekor/pkg/pki"
	"github.com/sigstore/rekor/pkg/types/rekord"

	"github.com/go-openapi/swag"
	"github.com/mitchellh/mapstructure"
	"github.com/sigstore/rekor/pkg/generated/models"
	"golang.org/x/sync/errgroup"
)

const (
	APIVERSION = "0.0.1"
)

func init() {
	rekord.SemVerToFacFnMap.Set(APIVERSION, NewEntry)
}

type V001Entry struct {
	RekordObj               models.RekordV001Schema
	fetchedExternalEntities bool
	keyObj                  pki.PublicKey
	sigObj                  pki.Signature
}

func (v V001Entry) APIVersion() string {
	return APIVERSION
}

func NewEntry() types.EntryImpl {
	return &V001Entry{}
}

func Base64StringtoByteArray() mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data interface{}) (interface{}, error) {
		if f.Kind() != reflect.String || t.Kind() != reflect.Slice {
			return data, nil
		}

		bytes, err := base64.StdEncoding.DecodeString(data.(string))
		if err != nil {
			return []byte{}, fmt.Errorf("failed parsing base64 data: %v", err)
		}
		return bytes, nil
	}
}

func (v V001Entry) IndexKeys() []string {
	var result []string

	if v.HasExternalEntities() {
		if err := v.FetchExternalEntities(context.Background()); err != nil {
			log.Logger.Error(err)
			return result
		}
	}

	key, err := v.keyObj.CanonicalValue()
	if err != nil {
		log.Logger.Error(err)
	} else {
		hasher := sha256.New()
		if _, err := hasher.Write(key); err != nil {
			log.Logger.Error(err)
		} else {
			result = append(result, strings.ToLower(hex.EncodeToString(hasher.Sum(nil))))
		}
	}

	if v.RekordObj.Data.Hash != nil {
		result = append(result, strings.ToLower(swag.StringValue(v.RekordObj.Data.Hash.Value)))
	}

	return result
}

func (v *V001Entry) Unmarshal(pe models.ProposedEntry) error {
	rekord, ok := pe.(*models.Rekord)
	if !ok {
		return errors.New("cannot unmarshal non Rekord v0.0.1 type")
	}

	cfg := mapstructure.DecoderConfig{
		DecodeHook: Base64StringtoByteArray(),
		Result:     &v.RekordObj,
	}

	dec, err := mapstructure.NewDecoder(&cfg)
	if err != nil {
		return fmt.Errorf("error initializing decoder: %w", err)
	}

	if err := dec.Decode(rekord.Spec); err != nil {
		return err
	}
	// field validation
	if err := v.RekordObj.Validate(strfmt.Default); err != nil {
		return err
	}
	// cross field validation
	return nil

}

func (v V001Entry) HasExternalEntities() bool {
	if v.fetchedExternalEntities {
		return false
	}

	if v.RekordObj.Data != nil && v.RekordObj.Data.URL.String() != "" {
		return true
	}
	if v.RekordObj.Signature != nil && v.RekordObj.Signature.URL.String() != "" {
		return true
	}
	if v.RekordObj.Signature != nil && v.RekordObj.Signature.PublicKey != nil && v.RekordObj.Signature.PublicKey.URL.String() != "" {
		return true
	}
	return false
}

func (v *V001Entry) FetchExternalEntities(ctx context.Context) error {
	if v.fetchedExternalEntities {
		return nil
	}

	if err := v.Validate(); err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)

	hashR, hashW := io.Pipe()
	sigR, sigW := io.Pipe()
	defer hashR.Close()
	defer sigR.Close()

	closePipesOnError := func(err error) error {
		pipeReaders := []*io.PipeReader{hashR, sigR}
		pipeWriters := []*io.PipeWriter{hashW, sigW}
		for idx := range pipeReaders {
			if e := pipeReaders[idx].CloseWithError(err); e != nil {
				log.Logger.Error(fmt.Errorf("error closing pipe: %w", e))
			}
			if e := pipeWriters[idx].CloseWithError(err); e != nil {
				log.Logger.Error(fmt.Errorf("error closing pipe: %w", e))
			}
		}
		return err
	}

	oldSHA := ""
	if v.RekordObj.Data.Hash != nil && v.RekordObj.Data.Hash.Value != nil {
		oldSHA = swag.StringValue(v.RekordObj.Data.Hash.Value)
	}
	artifactFactory := pki.NewArtifactFactory(v.RekordObj.Signature.Format)

	g.Go(func() error {
		defer hashW.Close()
		defer sigW.Close()

		dataReadCloser, err := util.FileOrURLReadCloser(ctx, v.RekordObj.Data.URL.String(), v.RekordObj.Data.Content)
		if err != nil {
			return closePipesOnError(err)
		}
		defer dataReadCloser.Close()

		/* #nosec G110 */
		if _, err := io.Copy(io.MultiWriter(hashW, sigW), dataReadCloser); err != nil {
			return closePipesOnError(err)
		}
		return nil
	})

	hashResult := make(chan string)

	g.Go(func() error {
		defer close(hashResult)
		hasher := sha256.New()

		if _, err := io.Copy(hasher, hashR); err != nil {
			return closePipesOnError(err)
		}

		computedSHA := hex.EncodeToString(hasher.Sum(nil))
		if oldSHA != "" && computedSHA != oldSHA {
			return closePipesOnError(fmt.Errorf("SHA mismatch: %s != %s", computedSHA, oldSHA))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case hashResult <- computedSHA:
			return nil
		}
	})

	sigResult := make(chan pki.Signature)

	g.Go(func() error {
		defer close(sigResult)

		sigReadCloser, err := util.FileOrURLReadCloser(ctx, v.RekordObj.Signature.URL.String(),
			v.RekordObj.Signature.Content)
		if err != nil {
			return closePipesOnError(err)
		}
		defer sigReadCloser.Close()

		signature, err := artifactFactory.NewSignature(sigReadCloser)
		if err != nil {
			return closePipesOnError(err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case sigResult <- signature:
			return nil
		}
	})

	keyResult := make(chan pki.PublicKey)

	g.Go(func() error {
		defer close(keyResult)

		keyReadCloser, err := util.FileOrURLReadCloser(ctx, v.RekordObj.Signature.PublicKey.URL.String(),
			v.RekordObj.Signature.PublicKey.Content)
		if err != nil {
			return closePipesOnError(err)
		}
		defer keyReadCloser.Close()

		key, err := artifactFactory.NewPublicKey(keyReadCloser)
		if err != nil {
			return closePipesOnError(err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case keyResult <- key:
			return nil
		}
	})

	g.Go(func() error {
		v.keyObj, v.sigObj = <-keyResult, <-sigResult

		if v.keyObj == nil || v.sigObj == nil {
			return closePipesOnError(errors.New("failed to read signature or public key"))
		}

		var err error
		if err = v.sigObj.Verify(sigR, v.keyObj); err != nil {
			return closePipesOnError(err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	})

	computedSHA := <-hashResult

	if err := g.Wait(); err != nil {
		return err
	}

	// if we get here, all goroutines succeeded without error
	if oldSHA == "" {
		v.RekordObj.Data.Hash = &models.RekordV001SchemaDataHash{}
		v.RekordObj.Data.Hash.Algorithm = swag.String(models.RekordV001SchemaDataHashAlgorithmSha256)
		v.RekordObj.Data.Hash.Value = swag.String(computedSHA)
	}

	v.fetchedExternalEntities = true
	return nil
}

func (v *V001Entry) Canonicalize(ctx context.Context) ([]byte, error) {
	if err := v.FetchExternalEntities(ctx); err != nil {
		return nil, err
	}
	if v.sigObj == nil {
		return nil, errors.New("signature object not initialized before canonicalization")
	}
	if v.keyObj == nil {
		return nil, errors.New("key object not initialized before canonicalization")
	}

	canonicalEntry := models.RekordV001Schema{}
	canonicalEntry.ExtraData = v.RekordObj.ExtraData

	// need to canonicalize signature & key content
	canonicalEntry.Signature = &models.RekordV001SchemaSignature{}
	// signature URL (if known) is not set deliberately
	canonicalEntry.Signature.Format = v.RekordObj.Signature.Format

	var err error
	canonicalEntry.Signature.Content, err = v.sigObj.CanonicalValue()
	if err != nil {
		return nil, err
	}

	// key URL (if known) is not set deliberately
	canonicalEntry.Signature.PublicKey = &models.RekordV001SchemaSignaturePublicKey{}
	canonicalEntry.Signature.PublicKey.Content, err = v.keyObj.CanonicalValue()
	if err != nil {
		return nil, err
	}

	canonicalEntry.Data = &models.RekordV001SchemaData{}
	canonicalEntry.Data.Hash = v.RekordObj.Data.Hash
	// data content is not set deliberately

	// ExtraData is copied through unfiltered
	canonicalEntry.ExtraData = v.RekordObj.ExtraData

	// wrap in valid object with kind and apiVersion set
	rekordObj := models.Rekord{}
	rekordObj.APIVersion = swag.String(APIVERSION)
	rekordObj.Spec = &canonicalEntry

	bytes, err := json.Marshal(&rekordObj)
	if err != nil {
		return nil, err
	}

	return bytes, nil
}

//Validate performs cross-field validation for fields in object
func (v V001Entry) Validate() error {

	sig := v.RekordObj.Signature
	if sig == nil {
		return errors.New("missing signature")
	}
	if len(sig.Content) == 0 && sig.URL.String() == "" {
		return errors.New("one of 'content' or 'url' must be specified for signature")
	}

	key := sig.PublicKey
	if key == nil {
		return errors.New("missing public key")
	}
	if len(key.Content) == 0 && key.URL.String() == "" {
		return errors.New("one of 'content' or 'url' must be specified for publicKey")
	}

	data := v.RekordObj.Data
	if data == nil {
		return errors.New("missing data")
	}

	if len(data.Content) == 0 && data.URL.String() == "" {
		return errors.New("one of 'content' or 'url' must be specified for data")
	}

	hash := data.Hash
	if hash != nil {
		if !govalidator.IsHash(swag.StringValue(hash.Value), swag.StringValue(hash.Algorithm)) {
			return errors.New("invalid value for hash")
		}
	}

	return nil
}
