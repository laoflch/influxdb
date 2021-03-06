package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/influxdata/influxdb"
)

// TODO: eradicate this with migration strategy
var variableOrgsIndex = []byte("variableorgsv1")

func (s *Service) initializeVariablesOrgIndex(tx Tx) error {
	if _, err := tx.Bucket(variableOrgsIndex); err != nil {
		return err
	}
	return nil
}

func decodeVariableOrgsIndexKey(indexKey []byte) (orgID influxdb.ID, variableID influxdb.ID, err error) {
	if len(indexKey) != 2*influxdb.IDLength {
		return 0, 0, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "malformed variable orgs index key (please report this error)",
		}
	}

	if err := (&orgID).Decode(indexKey[:influxdb.IDLength]); err != nil {
		return 0, 0, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "bad org id",
			Err:  influxdb.ErrInvalidID,
		}
	}

	if err := (&variableID).Decode(indexKey[influxdb.IDLength:]); err != nil {
		return 0, 0, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "bad variable id",
			Err:  influxdb.ErrInvalidID,
		}
	}

	return orgID, variableID, nil
}

func (s *Service) findOrganizationVariables(ctx context.Context, tx Tx, orgID influxdb.ID) ([]*influxdb.Variable, error) {
	idx, err := tx.Bucket(variableOrgsIndex)
	if err != nil {
		return nil, err
	}

	// TODO(leodido): support find options
	cur, err := idx.Cursor()
	if err != nil {
		return nil, err
	}

	prefix, err := orgID.Encode()
	if err != nil {
		return nil, err
	}

	variables := []*influxdb.Variable{}
	for k, _ := cur.Seek(prefix); bytes.HasPrefix(k, prefix); k, _ = cur.Next() {
		_, id, err := decodeVariableOrgsIndexKey(k)
		if err != nil {
			return nil, err
		}

		m, err := s.findVariableByID(ctx, tx, id)
		if err != nil {
			return nil, err
		}

		variables = append(variables, m)
	}

	return variables, nil
}

func newVariableStore() *IndexStore {
	const resource = "variable"

	var decodeVarEntFn DecodeBucketValFn = func(key, val []byte) ([]byte, interface{}, error) {
		var v influxdb.Variable
		return key, &v, json.Unmarshal(val, &v)
	}

	var decValToEntFn ConvertValToEntFn = func(_ []byte, i interface{}) (entity Entity, err error) {
		v, ok := i.(*influxdb.Variable)
		if err := errUnexpectedDecodeVal(ok); err != nil {
			return Entity{}, err
		}
		return Entity{
			PK:        EncID(v.ID),
			UniqueKey: Encode(EncID(v.OrganizationID), EncStringCaseInsensitive(v.Name)),
			Body:      v,
		}, nil
	}

	return &IndexStore{
		Resource:   resource,
		EntStore:   NewStoreBase(resource, []byte("variablesv1"), EncIDKey, EncBodyJSON, decodeVarEntFn, decValToEntFn),
		IndexStore: NewOrgNameKeyStore(resource, []byte("variablesindexv1"), true),
	}
}

func (s *Service) findVariables(ctx context.Context, tx Tx, filter influxdb.VariableFilter, opt ...influxdb.FindOptions) ([]*influxdb.Variable, error) {
	if filter.OrganizationID != nil {
		return s.findOrganizationVariables(ctx, tx, *filter.OrganizationID)
	}

	if filter.Organization != nil {
		o, err := s.findOrganizationByName(ctx, tx, *filter.Organization)
		if err != nil {
			return nil, err
		}
		return s.findOrganizationVariables(ctx, tx, o.ID)
	}

	var o influxdb.FindOptions
	if len(opt) > 0 {
		o = opt[0]
	}

	// TODO(jsteenb2): investigate why we don't implement the find options for vars?
	variables := make([]*influxdb.Variable, 0)
	err := s.variableStore.Find(ctx, tx, FindOpts{
		Descending:  o.Descending,
		Limit:       o.Limit,
		Offset:      o.Offset,
		FilterEntFn: filterVariablesFn(filter),
		CaptureFn: func(key []byte, decodedVal interface{}) error {
			variables = append(variables, decodedVal.(*influxdb.Variable))
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return variables, nil
}

func filterVariablesFn(filter influxdb.VariableFilter) func([]byte, interface{}) bool {
	return func(key []byte, val interface{}) bool {
		variable, ok := val.(*influxdb.Variable)
		if !ok {
			return false
		}

		if filter.ID != nil {
			return variable.ID == *filter.ID
		}

		if filter.OrganizationID != nil {
			return variable.OrganizationID == *filter.OrganizationID
		}

		return true
	}
}

// FindVariables returns all variables in the store
func (s *Service) FindVariables(ctx context.Context, filter influxdb.VariableFilter, opt ...influxdb.FindOptions) ([]*influxdb.Variable, error) {
	// todo(leodido) > handle find options
	res := []*influxdb.Variable{}
	err := s.kv.View(ctx, func(tx Tx) error {
		variables, err := s.findVariables(ctx, tx, filter, opt...)
		if err != nil && influxdb.ErrorCode(err) != influxdb.ENotFound {
			return err
		}
		res = variables
		return nil
	})
	return res, err
}

// FindVariableByID finds a single variable in the store by its ID
func (s *Service) FindVariableByID(ctx context.Context, id influxdb.ID) (*influxdb.Variable, error) {
	var variable *influxdb.Variable
	err := s.kv.View(ctx, func(tx Tx) error {
		m, err := s.findVariableByID(ctx, tx, id)
		if err != nil {
			return err
		}
		variable = m
		return nil
	})
	return variable, err
}

func (s *Service) findVariableByID(ctx context.Context, tx Tx, id influxdb.ID) (*influxdb.Variable, error) {
	body, err := s.variableStore.FindEnt(ctx, tx, Entity{PK: EncID(id)})
	if err != nil {
		return nil, err
	}

	variable, ok := body.(*influxdb.Variable)
	return variable, errUnexpectedDecodeVal(ok)
}

// CreateVariable creates a new variable and assigns it an ID
func (s *Service) CreateVariable(ctx context.Context, v *influxdb.Variable) error {
	return s.kv.Update(ctx, func(tx Tx) error {
		if err := v.Valid(); err != nil {
			return &influxdb.Error{
				Code: influxdb.EInvalid,
				Err:  err,
			}
		}

		v.Name = strings.TrimSpace(v.Name) // TODO: move to service layer

		_, err := s.variableStore.FindEnt(ctx, tx, Entity{
			UniqueKey: Encode(EncID(v.OrganizationID), EncStringCaseInsensitive(v.Name)),
		})
		if err == nil {
			return &influxdb.Error{
				Code: influxdb.EConflict,
				Msg:  fmt.Sprintf("variable with name %s already exists", v.Name),
			}
		}

		v.ID = s.IDGenerator.ID()

		now := s.Now()
		v.CreatedAt = now
		v.UpdatedAt = now
		return s.putVariable(ctx, tx, v)
	})
}

// ReplaceVariable puts a variable in the store
func (s *Service) ReplaceVariable(ctx context.Context, v *influxdb.Variable) error {
	return s.kv.Update(ctx, func(tx Tx) error {
		_, err := s.variableStore.FindEnt(ctx, tx, Entity{
			PK:        EncID(v.ID),
			UniqueKey: Encode(EncID(v.OrganizationID), EncStringCaseInsensitive(v.Name)),
		})
		if err == nil {
			return &influxdb.Error{
				Code: influxdb.EConflict,
				Msg:  fmt.Sprintf("variable with name %s already exists", v.Name),
			}
		}

		return s.putVariable(ctx, tx, v)
	})
}

func (s *Service) putVariable(ctx context.Context, tx Tx, v *influxdb.Variable) error {
	if err := s.putVariableOrgsIndex(tx, v); err != nil {
		return err
	}

	ent := Entity{
		PK:        EncID(v.ID),
		UniqueKey: Encode(EncID(v.OrganizationID), EncStringCaseInsensitive(v.Name)),
		Body:      v,
	}
	return s.variableStore.Put(ctx, tx, ent)
}

// UpdateVariable updates a single variable in the store with a changeset
func (s *Service) UpdateVariable(ctx context.Context, id influxdb.ID, update *influxdb.VariableUpdate) (*influxdb.Variable, error) {
	var v *influxdb.Variable
	err := s.kv.Update(ctx, func(tx Tx) error {
		m, err := s.findVariableByID(ctx, tx, id)
		if err != nil {
			return err
		}
		m.UpdatedAt = s.Now()

		v = m

		if update.Name != "" {
			// TODO: should be moved to service layer
			update.Name = strings.ToLower(strings.TrimSpace(update.Name))

			vbytes, err := s.variableStore.FindEnt(ctx, tx, Entity{
				UniqueKey: Encode(EncID(v.OrganizationID), EncStringCaseInsensitive(update.Name)),
			})
			if err == nil {
				existingVar, ok := vbytes.(*influxdb.Variable)
				if err := errUnexpectedDecodeVal(ok); err != nil {
					return err
				}
				if existingVar.ID != v.ID {
					return &influxdb.Error{
						Code: influxdb.EConflict,
						Msg:  fmt.Sprintf("variable with name %s already exists", update.Name),
					}
				}
			}

			err = s.variableStore.IndexStore.DeleteEnt(ctx, tx, Entity{
				UniqueKey: Encode(EncID(v.OrganizationID), EncStringCaseInsensitive(update.Name)),
			})
			if err != nil {
				return err
			}
			v.Name = update.Name
		}

		update.Apply(m)

		return s.putVariable(ctx, tx, v)
	})

	return v, err
}

// DeleteVariable removes a single variable from the store by its ID
func (s *Service) DeleteVariable(ctx context.Context, id influxdb.ID) error {
	return s.kv.Update(ctx, func(tx Tx) error {
		v, err := s.findVariableByID(ctx, tx, id)
		if err != nil {
			return err
		}

		if err := s.removeVariableOrgsIndex(tx, v); err != nil {
			return err
		}
		return s.variableStore.DeleteEnt(ctx, tx, Entity{PK: EncID(id)})
	})
}

func encodeVariableOrgsIndex(variable *influxdb.Variable) ([]byte, error) {
	oID, err := variable.OrganizationID.Encode()
	if err != nil {
		return nil, &influxdb.Error{
			Err: err,
			Msg: "bad organization id",
		}
	}

	mID, err := variable.ID.Encode()
	if err != nil {
		return nil, &influxdb.Error{
			Err: err,
			Msg: "bad variable id",
		}
	}

	key := make([]byte, 0, influxdb.IDLength*2)
	key = append(key, oID...)
	key = append(key, mID...)

	return key, nil
}

func (s *Service) putVariableOrgsIndex(tx Tx, variable *influxdb.Variable) error {
	key, err := encodeVariableOrgsIndex(variable)
	if err != nil {
		return err
	}

	idx, err := tx.Bucket(variableOrgsIndex)
	if err != nil {
		return &influxdb.Error{Code: influxdb.EInternal, Err: err}
	}

	if err := idx.Put(key, nil); err != nil {
		return &influxdb.Error{Code: influxdb.EInternal, Err: err}
	}

	return nil
}

func (s *Service) removeVariableOrgsIndex(tx Tx, variable *influxdb.Variable) error {
	key, err := encodeVariableOrgsIndex(variable)
	if err != nil {
		return err
	}

	idx, err := tx.Bucket(variableOrgsIndex)
	if err != nil {
		return &influxdb.Error{Code: influxdb.EInternal, Err: err}
	}

	if err := idx.Delete(key); err != nil {
		return &influxdb.Error{Code: influxdb.EInternal, Err: err}
	}

	return nil
}
