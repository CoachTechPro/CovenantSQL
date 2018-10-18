/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package interfaces

import (
	"reflect"
	"sync"

	"github.com/CovenantSQL/CovenantSQL/utils"
	"github.com/pkg/errors"
	"github.com/ugorji/go/codec"
)

const (
	// msgpack constants, copied from go/codec/msgpack.go
	valueTypeArray = 10
)

var (
	txTypeMapping sync.Map
	txType        = reflect.TypeOf((*Transaction)(nil)).Elem()
	txWrapperType = reflect.TypeOf((*TransactionWrapper)(nil))

	// ErrInvalidContainerType represents invalid container type read from msgpack bytes.
	ErrInvalidContainerType = errors.New("invalid container type for TransactionWrapper")
	// ErrInvalidTransactionType represents invalid transaction type read from msgpack bytes.
	ErrInvalidTransactionType = errors.New("invalid transaction type, can not instantiate transaction")
	// ErrTransactionRegistration represents invalid transaction object type being registered.
	ErrTransactionRegistration = errors.New("transaction register failed")
	// ErrMsgPackVersionMismatch represents the msgpack library abi has changed.
	ErrMsgPackVersionMismatch = errors.New("msgpack library version mismatch")
)

func init() {
	// detect msgpack version
	if codec.GenVersion != 8 {
		panic(ErrMsgPackVersionMismatch)
	}

	// register transaction wrapper to msgpack handler
	if err := utils.RegisterInterfaceToMsgPack(txType, txWrapperType); err != nil {
		panic(err)
	}
}

// TransactionWrapper is the wrapper for Transaction interface for serialization/deserialization purpose.
type TransactionWrapper struct {
	Transaction
}

// CodecEncodeSelf implements codec.Selfer interface.
func (w *TransactionWrapper) CodecEncodeSelf(e *codec.Encoder) {
	helperEncoder, encDriver := codec.GenHelperEncoder(e)

	if w == nil || w.Transaction == nil {
		encDriver.EncodeNil()
		return
	}

	// translate wrapper to two fields array wrapped by map
	encDriver.WriteArrayStart(2)
	encDriver.WriteArrayElem()
	encDriver.EncodeUint(uint64(w.GetTransactionType()))
	encDriver.WriteArrayElem()
	helperEncoder.EncFallback(w.Transaction)
	encDriver.WriteArrayEnd()
}

// CodecDecodeSelf implements codec.Selfer interface.
func (w *TransactionWrapper) CodecDecodeSelf(d *codec.Decoder) {
	helperDecoder, decodeDriver := codec.GenHelperDecoder(d)

	// clear fields
	w.Transaction = nil

	if ct := decodeDriver.ContainerType(); ct != valueTypeArray {
		panic(errors.Wrapf(ErrInvalidContainerType, "type %v applied", ct))
	}

	containerLen := decodeDriver.ReadArrayStart()

	for i := 0; i < containerLen; i++ {
		if decodeDriver.CheckBreak() {
			break
		}

		decodeDriver.ReadArrayElem()

		if i == 0 {
			if decodeDriver.TryDecodeAsNil() {
				// invalid type, can not instantiate transaction
				panic(ErrInvalidTransactionType)
			} else {
				txType := (TransactionType)(helperDecoder.C.UintV(decodeDriver.DecodeUint64(), 32))

				var err error
				if w.Transaction, err = NewTransaction(txType); err != nil {
					panic(err)
				}
			}
		} else if i == 1 {
			if !decodeDriver.TryDecodeAsNil() {
				helperDecoder.DecFallback(&w.Transaction, true)
			}
		} else {
			helperDecoder.DecStructFieldNotFound(i, "")
		}
	}

	decodeDriver.ReadArrayEnd()
}

// RegisterTransaction registers transaction type to wrapper.
func RegisterTransaction(t TransactionType, tx Transaction) {
	if tx == nil {
		panic(ErrTransactionRegistration)
	}
	rt := reflect.TypeOf(tx)

	if rt == txWrapperType {
		panic(ErrTransactionRegistration)
	}

	txTypeMapping.Store(t, rt)
}

// NewTransaction instantiates new transaction object.
func NewTransaction(t TransactionType) (tx Transaction, err error) {
	var d interface{}
	var ok bool
	var rt reflect.Type

	if d, ok = txTypeMapping.Load(t); !ok {
		err = errors.Wrapf(ErrInvalidTransactionType, "transaction not registered")
		return
	}
	rt = d.(reflect.Type)

	if !rt.Implements(txType) || rt == txWrapperType {
		err = errors.Wrap(ErrInvalidTransactionType, "invalid transaction registered")
		return
	}

	var rv reflect.Value

	if rt.Kind() == reflect.Ptr {
		rv = reflect.New(rt.Elem())
	} else {
		rv = reflect.New(rt).Elem()
	}

	tx = rv.Interface().(Transaction)

	return
}

// WrapTransaction wraps transaction in wrapper.
func WrapTransaction(tx Transaction) *TransactionWrapper {
	return &TransactionWrapper{
		Transaction: tx,
	}
}