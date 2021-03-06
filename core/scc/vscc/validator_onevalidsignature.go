/*
Copyright IBM Corp. 2016 All Rights Reserved.

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

package vscc

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/common/cauthdsl"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/chaincode/shim"
	"github.com/hyperledger/fabric/core/scc/lscc"
	mspmgmt "github.com/hyperledger/fabric/msp/mgmt"
	"github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
	"github.com/hyperledger/fabric/protos/utils"
)

var logger = flogging.MustGetLogger("vscc")

// ValidatorOneValidSignature implements the default transaction validation policy,
// which is to check the correctness of the read-write set and the endorsement
// signatures
type ValidatorOneValidSignature struct {
}

// Init is called once when the chaincode started the first time
func (vscc *ValidatorOneValidSignature) Init(stub shim.ChaincodeStubInterface) pb.Response {
	// best practice to do nothing (or very little) in Init
	return shim.Success(nil)
}

// Invoke is called to validate the specified block of transactions
// This validation system chaincode will check the read-write set validity and at least 1
// correct endorsement. Later we can create more validation system
// chaincodes to provide more sophisticated policy processing such as enabling
// policy specification to be coded as a transaction of the chaincode and the client
// selecting which policy to use for validation using parameter function
// @return serialized Block of valid and invalid transactions indentified
// Note that Peer calls this function with 3 arguments, where args[0] is the
// function name, args[1] is the Envelope and args[2] is the validation policy
func (vscc *ValidatorOneValidSignature) Invoke(stub shim.ChaincodeStubInterface) pb.Response {
	// TODO: document the argument in some white paper or design document
	// args[0] - function name (not used now)
	// args[1] - serialized Envelope
	// args[2] - serialized policy
	args := stub.GetArgs()
	if len(args) < 3 {
		return shim.Error("Incorrect number of arguments")
	}

	if args[1] == nil {
		return shim.Error("No block to validate")
	}

	if args[2] == nil {
		return shim.Error("No policy supplied")
	}

	logger.Debugf("VSCC invoked")

	// get the envelope...
	env, err := utils.GetEnvelopeFromBlock(args[1])
	if err != nil {
		logger.Errorf("VSCC error: GetEnvelope failed, err %s", err)
		return shim.Error(err.Error())
	}

	// ...and the payload...
	payl, err := utils.GetPayload(env)
	if err != nil {
		logger.Errorf("VSCC error: GetPayload failed, err %s", err)
		return shim.Error(err.Error())
	}

	chdr, err := utils.UnmarshalChannelHeader(payl.Header.ChannelHeader)
	if err != nil {
		return shim.Error(err.Error())
	}

	// get the policy
	mgr := mspmgmt.GetManagerForChain(chdr.ChannelId)
	pProvider := cauthdsl.NewPolicyProvider(mgr)
	policy, _, err := pProvider.NewPolicy(args[2])
	if err != nil {
		logger.Errorf("VSCC error: pProvider.NewPolicy failed, err %s", err)
		return shim.Error(err.Error())
	}

	// validate the payload type
	if common.HeaderType(chdr.Type) != common.HeaderType_ENDORSER_TRANSACTION {
		logger.Errorf("Only Endorser Transactions are supported, provided type %d", chdr.Type)
		return shim.Error(fmt.Sprintf("Only Endorser Transactions are supported, provided type %d", chdr.Type))
	}

	// ...and the transaction...
	tx, err := utils.GetTransaction(payl.Data)
	if err != nil {
		logger.Errorf("VSCC error: GetTransaction failed, err %s", err)
		return shim.Error(err.Error())
	}

	// loop through each of the actions within
	for _, act := range tx.Actions {
		cap, err := utils.GetChaincodeActionPayload(act.Payload)
		if err != nil {
			logger.Errorf("VSCC error: GetChaincodeActionPayload failed, err %s", err)
			return shim.Error(err.Error())
		}

		// this is the first part of the signed message
		prespBytes := cap.Action.ProposalResponsePayload
		// build the signature set for the evaluation
		signatureSet := make([]*common.SignedData, len(cap.Action.Endorsements))

		// loop through each of the endorsements and build the signature set
		for i, endorsement := range cap.Action.Endorsements {
			signatureSet[i] = &common.SignedData{
				// set the data that is signed; concatenation of proposal response bytes and endorser ID
				Data: append(prespBytes, endorsement.Endorser...),
				// set the identity that signs the message: it's the endorser
				Identity: endorsement.Endorser,
				// set the signature
				Signature: endorsement.Signature,
			}
		}

		// evaluate the signature set against the policy
		err = policy.Evaluate(signatureSet)
		if err != nil {
			return shim.Error(fmt.Sprintf("VSCC error: policy evaluation failed, err %s", err))
		}

		hdrExt, err := utils.GetChaincodeHeaderExtension(payl.Header)
		if err != nil {
			logger.Errorf("VSCC error: GetChaincodeHeaderExtension failed, err %s", err)
			return shim.Error(err.Error())
		}

		// do some extra validation that is specific to lscc
		if hdrExt.ChaincodeId.Name == "lscc" {
			err = vscc.ValidateLSCCInvocation(cap)
			if err != nil {
				logger.Errorf("VSCC error: ValidateLSCCInvocation failed, err %s", err)
				return shim.Error(err.Error())
			}
		}
	}

	logger.Debugf("VSCC exists successfully")

	return shim.Success(nil)
}

func (vscc *ValidatorOneValidSignature) ValidateLSCCInvocation(cap *pb.ChaincodeActionPayload) error {
	cpp, err := utils.GetChaincodeProposalPayload(cap.ChaincodeProposalPayload)
	if err != nil {
		logger.Errorf("VSCC error: GetChaincodeProposalPayload failed, err %s", err)
		return err
	}

	cis := &pb.ChaincodeInvocationSpec{}
	err = proto.Unmarshal(cpp.Input, cis)
	if err != nil {
		logger.Errorf("VSCC error: Unmarshal ChaincodeInvocationSpec failed, err %s", err)
		return err
	}

	if cis == nil ||
		cis.ChaincodeSpec == nil ||
		cis.ChaincodeSpec.Input == nil ||
		cis.ChaincodeSpec.Input.Args == nil {
		logger.Errorf("VSCC error: committing invalid vscc invocation")
		return fmt.Errorf("VSCC error: committing invalid vscc invocation")
	}

	lsccFunc := string(cis.ChaincodeSpec.Input.Args[0])
	lsccArgs := cis.ChaincodeSpec.Input.Args[1:]

	switch lsccFunc {
	case lscc.DEPLOY:
	case lscc.UPGRADE:
		logger.Infof("VSCC info: validating invocation of lscc function %s on arguments %#v", lsccFunc, lsccArgs)

		// TODO: two more crs are expected to fill this gap, as explained in FAB-3155
		// 1) check that the invocation complies with the InstantiationPolicy
		// 2) check that the read/write set is appropriate

		return nil
	default:
		return fmt.Errorf("VSCC error: committing an invocation of function %s of lscc is invalid", lsccFunc)
	}

	return nil
}
