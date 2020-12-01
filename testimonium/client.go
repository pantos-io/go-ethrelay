// This file contains functions called by the various commands. These functions are used to interact with smart contracts
// (Ethash, Testimonium)
// Authors: Marten Sigwart, Philipp Frauenthaler

package testimonium

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/pantos-io/go-testimonium/ethereum/ethash"
	"github.com/pantos-io/go-testimonium/typedefs"
)

type ChainConfig map[string]interface{}

type ChainsConfig map[uint8]ChainConfig

type Chain struct {
	client                     *ethclient.Client
	testimoniumContractAddress common.Address
	testimoniumContract        *Testimonium
	ethashContractAddress      common.Address
	ethashContract             *ethash.Ethash
	fullUrl                    string
}

type Client struct {
	chains     map[uint8]*Chain
	account    common.Address
	privateKey *ecdsa.PrivateKey
}

type Header struct {
	Hash                      [32]byte
	BlockNumber               *big.Int
	TotalDifficulty           *big.Int
}

type FullHeader struct {
	Parent                    [32]byte
	UncleHash                 [32]byte
	StateRoot                 [32]byte
	TransactionsRoot          [32]byte
	ReceiptsRoot              [32]byte
	BlockNumber               *big.Int
	GasLimit                  *big.Int
	GasUsed                   *big.Int
	RlpHeaderHashWithoutNonce [32]byte
	Timestamp                 *big.Int
	Nonce                     *big.Int
	Difficulty                *big.Int
	ExtraData                 *byte
}

type VerificationResult struct {
	returnCode uint8
}

type TrieValueType int

const (
	VALUE_TYPE_TRANSACTION TrieValueType = 0
	VALUE_TYPE_RECEIPT     TrieValueType = 1
	VALUE_TYPE_STATE       TrieValueType = 2
)

func (header FullHeader) String() string {
	return fmt.Sprintf(`BlockHeader: {
Parent: %s,
StateRoot: %s,
TransactionsRoot: %s,
ReceiptsRoot: %s,
BlockNumber: %s,
RlpHeaderHashWithoutNonce: %s,
Nonce: %s,
Difficulty: %s }`,
		common.Bytes2Hex(header.Parent[:]),
		common.Bytes2Hex(header.StateRoot[:]),
		common.Bytes2Hex(header.TransactionsRoot[:]),
		common.Bytes2Hex(header.ReceiptsRoot[:]),
		header.BlockNumber.String(),
		common.Bytes2Hex(header.RlpHeaderHashWithoutNonce[:]),
		header.Nonce.String(),
		header.Difficulty.String())
}

func (t TestimoniumSubmitHeader) String() string {
	return fmt.Sprintf("SubmitHeaderEvent: { Hash: %s }", common.BytesToHash(t.BlockHash[:]).String())
}

func (event TestimoniumRemoveBranch) String() string {
	return fmt.Sprintf("RemoveBranchEvent: { Root: %s }", common.BytesToHash(event.Root[:]).String())
}

func (event TestimoniumPoWValidationResult) String() string {
	return fmt.Sprintf("PoWValidationResultEvent: { returnCode: %d, errorInfo: %d }", event.ReturnCode, event.ErrorInfo)
}

func (result VerificationResult) String() string {
	return fmt.Sprintf("VerificationResult: { returnCode: %d }", result.returnCode)
}

func (event TestimoniumWithdrawStake) String() string {
	return fmt.Sprintf("TestimoniumWithdrawStakeEvent: { client: %s, withdrawnStake: %d }", common.Bytes2Hex(event.Client.Bytes()), event.WithdrawnStake)
}

func CreateChainConfig(connectionType string, connectionUrl string, connectionPort uint64) map[string]interface{} {
	chainConfig := make(map[string]interface{})

	chainConfig["url"] = connectionUrl

	if connectionType != "" {
		chainConfig["type"] = connectionType
	}
	if connectionPort != 0 {
		chainConfig["port"] = connectionPort
	}
	return chainConfig
}

func NewClient(privateKey string, chainsConfig map[string]interface{}) *Client {
	client := new(Client)
	client.chains = make(map[uint8]*Chain)

	for k, v := range chainsConfig {
		chainId, err := strconv.ParseInt(k, 10, 8)
		if err != nil {
			log.Fatal(err)
		}

		chainConfig := v.(map[string]interface{})

		// create client connection
		var ethClient *ethclient.Client
		fullUrl, err := createConnectionUrl(chainConfig)
		if err != nil {
			fmt.Printf("WARNING: Could not read url specified for chain %d (%s)\n", chainId, err)
			continue
		}

		ethClient, err = ethclient.Dial(fullUrl)
		if err != nil {
			fmt.Printf("WARNING: Cannot connect to chain %d (%s): %s\n", chainId, fullUrl, err)
			continue // --> even if we cannot connect to this chain, we still try to connect to the other ones
		}

		chain := new(Chain)
		chain.client = ethClient
		chain.fullUrl = fullUrl

		// create testimonium contract instance
		var testimoniumContract *Testimonium
		addressHex := chainConfig["testimoniumaddress"]
		if addressHex != nil {
			testimoniumAddress := common.HexToAddress(addressHex.(string))
			testimoniumContract, err = NewTestimonium(testimoniumAddress, ethClient)
			if err != nil {
				fmt.Printf("WARNING: No Testimonium contract deployed at address %s on chain %d (%s)\n", addressHex, chainId, fullUrl)
			} else {
				chain.testimoniumContract = testimoniumContract
				chain.testimoniumContractAddress = testimoniumAddress
			}
		}

		// create ethash contract instance
		var ethashContract *ethash.Ethash
		addressHex = chainConfig["ethashaddress"]
		if addressHex != nil {
			ethashAddress := common.HexToAddress(addressHex.(string))
			ethashContract, err = ethash.NewEthash(ethashAddress, ethClient)
			if err != nil {
				fmt.Printf("WARNING: No Ethash contract deployed at address %s on chain %d (%s)\n", addressHex, chainId, fullUrl)
			} else {
				chain.ethashContract = ethashContract
				chain.ethashContractAddress = ethashAddress
			}
		}

		client.chains[uint8(chainId)] = chain
	}

	// get public address
	privateKeyBytes, err := hexutil.Decode(privateKey)
	if err != nil {
		fmt.Println("Could not decode private key. Is it a correct hex string (0x...)?")
		os.Exit(1)
	}
	ecdsaPrivateKey, err := crypto.ToECDSA(privateKeyBytes)
	if err != nil {
		log.Fatal(err)
	}
	client.privateKey = ecdsaPrivateKey
	publicKey := ecdsaPrivateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("error casting public key to ECDSA")
	}

	client.account = crypto.PubkeyToAddress(*publicKeyECDSA)

	return client
}

func createConnectionUrl(chainConfig map[string]interface{}) (string, error) {
	fullUrl := ""
	if chainConfig["type"] != nil {
		fullUrl += chainConfig["type"].(string) + "://"
	} else {
		fullUrl += "https://"
	}
	if chainConfig["url"] == nil {
		return "", fmt.Errorf("no url specified")
	}

	fullUrl += chainConfig["url"].(string)
	if chainConfig["port"] != nil {
		// port can be parsed as int
		if port, ok := chainConfig["port"].(int); ok {
			fullUrl = fmt.Sprintf("%s:%d", fullUrl, port)
			return fullUrl, nil
		}

		// port is a string but could still be a legal port
		port, err := strconv.ParseUint(chainConfig["port"].(string), 10, 64)
		if err != nil {
			return "", fmt.Errorf("llegal port: %s", chainConfig["port"].(string))
		}
		fullUrl = fmt.Sprintf("%s:%d", fullUrl, port)
	}
	return fullUrl, nil
}

func (c Client) Chains() []uint8 {
	keys := make([]uint8, len(c.chains))

	i := 0
	for k := range c.chains {
		keys[i] = k
		i++
	}
	return keys
}

func (c Client) Account() string {
	return c.account.Hex()
}

func (c Client) TotalBalance() (*big.Int, error) {
	var totalBalance = new(big.Int)
	for k, _ := range c.chains {
		balance, err := c.Balance(k)
		if err != nil {
			return nil, err
		}
		totalBalance.Add(totalBalance, balance)
	}
	return totalBalance, nil
}

func (c Client) Balance(chainId uint8) (*big.Int, error) {
	var totalBalance = new(big.Int);

	_, exists := c.chains[chainId]
	if !exists {
		return nil, fmt.Errorf("chain %s does not exist", chainId)
	}

	balance, err := c.chains[chainId].client.BalanceAt(context.Background(), c.account, nil)
	if err != nil {
		return nil, err
	}

	totalBalance.Add(totalBalance, balance)

	return totalBalance, nil
}

func (c Client) GetStake(chainId uint8) (*big.Int, error) {
	_, exists := c.chains[chainId]
	if !exists {
		return nil, fmt.Errorf("chain %s does not exist", chainId)
	}
	stake, err := c.chains[chainId].testimoniumContract.GetStake(
		&bind.CallOpts{
			From: c.account,
		})
	if err != nil {
		return nil, err
	}
	return stake, nil
}

func (c Client) DepositStake(chainId uint8, amountInWei *big.Int) error {
	_, exists := c.chains[chainId]
	if !exists {
		return fmt.Errorf("chain %s does not exist", chainId)
	}

	auth := prepareTransaction(c.account, c.privateKey, c.chains[chainId], amountInWei)

	tx, err := c.chains[chainId].testimoniumContract.DepositStake(auth, amountInWei)
	if err != nil {
		return err
	}

	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	return nil
}

func (c Client) WithdrawStake(chainId uint8, amountInWei *big.Int) error {
	_, exists := c.chains[chainId]
	if !exists {
		return fmt.Errorf("chain %s does not exist", chainId)
	}
	auth := prepareTransaction(c.account, c.privateKey, c.chains[chainId], big.NewInt(0))
	tx, err := c.chains[chainId].testimoniumContract.WithdrawStake(auth, amountInWei)
	if err != nil {
		return err
	}
	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	receipt, err := awaitTxReceipt(c.chains[chainId].client, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status == 0 {
		// Transaction failed
		reason := getFailureReason(c.chains[chainId].client, c.account, tx, receipt.BlockNumber)
		return fmt.Errorf("Tx failed: %s\n", reason)
	}

	// Transaction is successful
	eventIterator, err := c.chains[chainId].testimoniumContract.TestimoniumFilterer.FilterWithdrawStake(&bind.FilterOpts{
		Start:   receipt.BlockNumber.Uint64(),
		End:     nil,
		Context: nil,
	})
	if err != nil {
		return err
	}

	if eventIterator.Next() {
		fmt.Printf("Tx successful: %s\n", eventIterator.Event.String())
	}

	return nil
}

func (c Client) BlockHeaderExists(blockHash [32]byte, chain uint8) (bool, error) {
	_, exists := c.chains[chain]
	if !exists {
		return false, fmt.Errorf("chain %s does not exist", chain)
	}

	return c.chains[chain].testimoniumContract.IsHeaderStored(nil, blockHash)
}

func (c Client) LongestChainEndpoint(chain uint8) ([32]byte, error) {
	_, exists := c.chains[chain]
	if !exists {
		fmt.Errorf("chain %s does not exist", chain)
	}

	return c.chains[chain].testimoniumContract.LongestChainEndpoint(nil)
}

func (c Client) GetBlockHeader(blockHash [32]byte, chain uint8) (Header, error) {
	return c.chains[chain].testimoniumContract.GetHeader(nil, blockHash)
}

func (c Client) GetOriginalBlockHeader(blockHash [32]byte, chain uint8) (*types.Block, error) {
	return c.chains[chain].client.BlockByHash(context.Background(), common.BytesToHash(blockHash[:]))
}

func (c Client) SubmitHeader(header *types.Header, chain uint8) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	rlpHeader, err := encodeHeaderToRLP(header)
	if err != nil {
		log.Fatal("Failed to encode header to RLP: " + err.Error())
	}

	c.SubmitRLPHeader(rlpHeader, chain)
}

func (c Client) SubmitRLPHeader(rlpHeader []byte, chain uint8) {
	// Check preconditions
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	// Submit Transfer Transaction
	auth := prepareTransaction(c.account, c.privateKey, c.chains[chain], big.NewInt(0))
	tx, err := c.chains[chain].testimoniumContract.SubmitBlock(auth, rlpHeader)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	receipt, err := awaitTxReceipt(c.chains[chain].client, tx.Hash())
	if err != nil {
		log.Fatal(err)
	}

	if receipt.Status == 0 {
		// Transaction failed
		reason := getFailureReason(c.chains[chain].client, c.account, tx, receipt.BlockNumber)
		fmt.Printf("Tx failed: %s\n", reason)
		return
	}

	// Transaction is successful
	eventIterator, err := c.chains[chain].testimoniumContract.TestimoniumFilterer.FilterSubmitHeader(&bind.FilterOpts{
		Start:   receipt.BlockNumber.Uint64(),
		End:     nil,
		Context: nil,
	})
	if err != nil {
		log.Fatal(err)
	}

	if eventIterator.Next() {
		fmt.Printf("Tx successful: %s\n", eventIterator.Event.String())
	}
}

func (c Client) BlockByHash(blockHash common.Hash, chain uint8) (*types.Block, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	return c.chains[chain].client.BlockByHash(context.Background(), blockHash)
}

func (c Client) BlockByNumber(blockNumber uint64, chain uint8) (*types.Block, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	return c.chains[chain].client.BlockByNumber(context.Background(), new(big.Int).SetUint64(blockNumber))
}

func (c Client) HeaderByNumber(blockNumber *big.Int, chain uint8) (*types.Header, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	return c.chains[chain].client.HeaderByNumber(context.Background(), blockNumber)
}

type TotalDifficulty struct {
	TotalDifficulty string `json:"totalDifficulty"       gencodec:"required"`
}

func toBlockNumArg(number *big.Int) string {
	if number == nil {
		return "latest"
	}

	return hexutil.EncodeBig(number)
}

func (c Client) TotalDifficulty(blockNumber *big.Int, chain uint8) (*big.Int, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	client, err := rpc.Dial(c.chains[chain].fullUrl)
	if err != nil {
		log.Fatal("Failed to connect to chain", err)
	}

	var totalDifficulty *TotalDifficulty
	err = client.CallContext(context.Background(), &totalDifficulty, "eth_getBlockByNumber", toBlockNumArg(blockNumber), false)
	if err == nil && totalDifficulty == nil {
		return big.NewInt(0), ethereum.NotFound
	}

	diff, err := hexutil.DecodeBig(totalDifficulty.TotalDifficulty)
	if err != nil {
		return big.NewInt(0), err
	}

	return diff, nil
}

func (c Client) HeaderByHash(blockHash common.Hash, chain uint8) (*types.Header, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	return c.chains[chain].client.HeaderByHash(context.Background(), blockHash)
}

func (c Client) Transaction(txHash common.Hash, chain uint8) (*types.Transaction, bool, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	return c.chains[chain].client.TransactionByHash(context.Background(), txHash)
}

func (c Client) TransactionReceipt(txHash common.Hash, chain uint8) (*types.Receipt, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	return c.chains[chain].client.TransactionReceipt(context.Background(), txHash)
}

func (c Client) RandomizeHeader(header *types.Header, chain uint8) *types.Header {
	temp := header.TxHash

	header.TxHash = header.ReceiptHash
	header.ReceiptHash = header.Root
	header.Root = temp

	return header
}

func getRlpHeaderByTestimoniumSubmitEvent(chain *Chain, blockHash [32]byte) ([]byte, error) {
	eventIterator, err := chain.testimoniumContract.FilterSubmitHeader(nil)
	if err != nil {
		return nil, err
	}

	// TODO: the search could be enhanced if we use index event parameters, but this causes a little more cost and changes to the contract, evaluation is necessary

	for eventIterator.Next() {
		// according to the contract, the submit header event has exactly one parameter/data-item that is submitted
		// as no block-hash can be submitted twice, we found an event if the event data does equal the block-hash
		if bytes.Equal(eventIterator.Event.Raw.Data, blockHash[:]) {

			// get the hash where the event was emitted
			txHash := eventIterator.Event.Raw.TxHash

			// get the full transaction by txhash
			tx, isPending, err := chain.client.TransactionByHash(context.Background(), txHash)
			if err != nil {
				return nil, err
			}

			// if the transaction is pending, we don't know if it will be included
			if isPending {
				return nil, fmt.Errorf("transaction where block was submitted is currently pending...")
			}

			// get raw abi-encoded bytes of transaction data
			txData := tx.Data()

			// parse method-id, the first 4 bytes are always the first 4 bytes of the encoded message signature
			methodId := txData[0:4]

			// compare method-id with abi to ensure correct contracts etc., else fail
			// TODO: this needs to be changed if the contract changes the submit-method name or their params
			//  this could be prevented in parsing the contract ABI and make your own signature of the header
			//  additionally, the parsing of the method params could be automated if we know the exact ABI content
			if !bytes.Equal(methodId, common.Hex2Bytes("d5107381")) {
				return nil, fmt.Errorf("signature of called method does not match contract method signature")
			}

			// get the position where the dynamic byte array (byte slice), containing the rlp encoded header, starts
			// this is encoded in the next 32 bytes after the 4 bytes of the method signature
			position := new(big.Int)
			position.SetBytes(txData[4:4 + 32])

			// as the rlp header is a dynamic byte array, we need to know the length of the content which is encoded
			// in the next 32 bytes - generally, variable content with a prepended length param is appended to the
			// end of the encoded, in this case it directly follows after the starting position of the first param
			length := new(big.Int)
			length.SetBytes(txData[4 + position.Uint64():4 + position.Uint64() + 32])

			// get the rlpHeader data from the transaction starting after all params and length params are parsed
			rlpEncodedBlockHeader := txData[4 + position.Uint64() + 32: 4 + position.Uint64() + 32 + length.Uint64()]

			return rlpEncodedBlockHeader, nil
		}
	}

	return nil, fmt.Errorf("no submit event for block '%s' found", common.Bytes2Hex(blockHash[:]))
}

func (c Client) DisputeBlock(blockHash [32]byte, chain uint8) {
	fmt.Println("Disputing block ...")

	rlpEncodedBlockHeader, err := getRlpHeaderByTestimoniumSubmitEvent(c.chains[chain], blockHash)
	if err != nil {
		log.Fatal(err)
	}

	// decode block header from rlp encoded block header
	blockHeader, err := decodeHeaderFromRLP(rlpEncodedBlockHeader)
	if err != nil {
		log.Fatal(err)
	}

	// take the encoded block header and encode it without the nonce and the mixed hash
	blockHeaderWithoutNonce, err := encodeHeaderWithoutNonceToRLP(blockHeader)
	if err != nil {
		log.Fatal(err)
	}

	// create a hash to get the block hash without nonce needed for the ethash metadata construction
	blockHeaderHashWithoutNonce := crypto.Keccak256(blockHeaderWithoutNonce)

	// keccak256 returns a dynamic byte array, but we need a 32 byte fixed size byte array for the ethash block meta data
	var blockHeaderHashWithoutNonceLength32 [32]byte
	copy(blockHeaderHashWithoutNonceLength32[:], blockHeaderHashWithoutNonce)

	// get DAG and compute dataSetLookup and witnessForLookup
	blockMetaData := ethash.NewBlockMetaData(blockHeader.Number.Uint64(), blockHeader.Nonce.Uint64(), blockHeaderHashWithoutNonceLength32)
	dataSetLookUp := blockMetaData.DAGElementArray()
	witnessForLookup := blockMetaData.DAGProofArray()

	// the last thing needed for calling dispute is the parent rlp encoded block header
	rlpEncodedParentBlockHeader, err := getRlpHeaderByTestimoniumSubmitEvent(c.chains[chain], blockHeader.ParentHash)
	if err != nil {
		log.Fatal(err)
	}

	auth := prepareTransaction(c.account, c.privateKey, c.chains[chain], big.NewInt(0))

	tx, err := c.chains[chain].testimoniumContract.DisputeBlockHeader(auth, rlpEncodedBlockHeader, rlpEncodedParentBlockHeader, dataSetLookUp, witnessForLookup)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	receipt, err := awaitTxReceipt(c.chains[chain].client, tx.Hash())
	if err != nil {
		log.Fatal(err)
	}

	if receipt.Status == 0 {
		// Transaction failed
		reason := getFailureReason(c.chains[chain].client, c.account, tx, receipt.BlockNumber)
		fmt.Printf("Tx failed: %s\n", reason)
		return
	}

	// get RemoveBranch event
	eventIteratorRemoveBranch, err := c.chains[chain].testimoniumContract.TestimoniumFilterer.FilterRemoveBranch(&bind.FilterOpts{
		Start:   receipt.BlockNumber.Uint64(),
		End:     nil,
		Context: nil,
	})
	if err != nil {
		log.Fatal(err)
	}

	if eventIteratorRemoveBranch.Next() {
		fmt.Printf("Tx successful: %s\n", eventIteratorRemoveBranch.Event.String())
	}

	// get PoW Verification event
	eventIteratorPoWResult, err := c.chains[chain].testimoniumContract.TestimoniumFilterer.FilterPoWValidationResult(&bind.FilterOpts{
		Start:   receipt.BlockNumber.Uint64(),
		End:     nil,
		Context: nil,
	})
	if err != nil {
		log.Fatal(err)
	}

	if eventIteratorPoWResult.Next() {
		fmt.Printf("Tx successful: %s\n", eventIteratorPoWResult.Event.String())
	}
}

func (c Client) GetRequiredVerificationFee(chain uint8) (*big.Int, error) {
	return c.chains[chain].testimoniumContract.GetRequiredVerificationFee(nil)
}

func (c Client) GenerateMerkleProofForTx(txHash [32]byte, chain uint8) ([]byte, []byte, []byte, []byte, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	txReceipt, err := c.chains[chain].client.TransactionReceipt(context.Background(), txHash)
	if err != nil {
		return []byte{}, []byte{}, []byte{}, []byte{}, err
	}

	block, err := c.chains[chain].client.BlockByHash(context.Background(), txReceipt.BlockHash)
	if err != nil {
		return []byte{}, []byte{}, []byte{}, []byte{}, err
	}

	// create transactions trie
	buffer := new(bytes.Buffer)
	merkleTrie := new(trie.Trie)
	txList := block.Transactions()
	for i := 0; i < txList.Len(); i++ {
		buffer.Reset()
		rlp.Encode(buffer, uint(i))
		merkleTrie.Update(buffer.Bytes(), txList.GetRlp(i))
	}

	// create Merkle proof
	rlpEncodedTx := txList.GetRlp(int(txReceipt.TransactionIndex))

	buffer.Reset()
	rlp.Encode(buffer, txReceipt.TransactionIndex)
	path := make([]byte, len(buffer.Bytes()))
	copy(path, buffer.Bytes())

	merkleIterator := merkleTrie.NodeIterator(nil)
	var proofNodes [][]byte
	for merkleIterator.Next(true) {
		if merkleIterator.Leaf() && bytes.Equal(merkleIterator.LeafKey(), path) {
			// leaf node representing tx has been found --> create Merkle proof
			proofNodes = merkleIterator.LeafProof()
			break
		}
	}

	buffer.Reset()
	rlp.Encode(buffer, proofNodes)
	rlpEncodedProofNodes := make([]byte, len(buffer.Bytes()))
	copy(rlpEncodedProofNodes, buffer.Bytes())

	buffer.Reset()
	rlp.Encode(buffer, block.Header())
	rlpEncodedHeader := make([]byte, len(buffer.Bytes()))
	copy(rlpEncodedHeader, buffer.Bytes())

	return rlpEncodedHeader, rlpEncodedTx, path, rlpEncodedProofNodes, nil
}

func (c Client) GenerateMerkleProofForReceipt(txHash [32]byte, chain uint8) ([]byte, []byte, []byte, []byte, error) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	txReceipt, err := c.chains[chain].client.TransactionReceipt(context.Background(), txHash)
	if err != nil {
		return []byte{}, []byte{}, []byte{}, []byte{}, err
	}

	block, err := c.chains[chain].client.BlockByHash(context.Background(), txReceipt.BlockHash)
	if err != nil {
		return []byte{}, []byte{}, []byte{}, []byte{}, err
	}

	var path []byte
	var rlpEncodedReceipt []byte

	// create receipts trie
	buffer := new(bytes.Buffer)
	merkleTrie := new(trie.Trie)
	for i := 0; i < block.Transactions().Len(); i++ {
		tx := block.Body().Transactions[i]

		receipt, err := c.chains[chain].client.TransactionReceipt(context.Background(), tx.Hash())
		if err != nil {
			return []byte{}, []byte{}, []byte{}, []byte{}, err
		}

		buffer.Reset()
		receipt.EncodeRLP(buffer)
		encodedReceipt := make([]byte, len(buffer.Bytes()))
		copy(encodedReceipt, buffer.Bytes())

		buffer.Reset()
		rlp.Encode(buffer, uint(i))

		if txReceipt.TxHash == receipt.TxHash {
			path = make([]byte, len(buffer.Bytes()))
			copy(path, buffer.Bytes())

			rlpEncodedReceipt = make([]byte, len(encodedReceipt))
			copy(rlpEncodedReceipt, encodedReceipt)
		}

		merkleTrie.Update(buffer.Bytes(), encodedReceipt)
	}

	// create Merkle proof

	merkleIterator := merkleTrie.NodeIterator(nil)
	var proofNodes [][]byte
	for merkleIterator.Next(true) {
		if merkleIterator.Leaf() && bytes.Equal(merkleIterator.LeafKey(), path) {
			// leaf node representing tx has been found --> create Merkle proof
			proofNodes = merkleIterator.LeafProof()
			break
		}
	}

	buffer.Reset()
	rlp.Encode(buffer, proofNodes)
	rlpEncodedProofNodes := make([]byte, len(buffer.Bytes()))
	copy(rlpEncodedProofNodes, buffer.Bytes())

	buffer.Reset()
	rlp.Encode(buffer, block.Header())
	rlpEncodedHeader := make([]byte, len(buffer.Bytes()))
	copy(rlpEncodedHeader, buffer.Bytes())

	return rlpEncodedHeader, rlpEncodedReceipt, path, rlpEncodedProofNodes, nil
}

func (c Client) VerifyMerkleProof(feeInWei *big.Int, rlpHeader []byte, trieValueType TrieValueType, rlpEncodedValue []byte, path []byte,
	rlpEncodedProofNodes []byte, noOfConfirmations uint8, chain uint8) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	var tx *types.Transaction
	var err error
	auth := prepareTransaction(c.account, c.privateKey, c.chains[chain], feeInWei)

	switch trieValueType {
		case VALUE_TYPE_TRANSACTION:
			tx, err = c.chains[chain].testimoniumContract.VerifyTransaction(auth, feeInWei, rlpHeader,
				noOfConfirmations, rlpEncodedValue, path, rlpEncodedProofNodes)
		case VALUE_TYPE_RECEIPT:
			tx, err = c.chains[chain].testimoniumContract.VerifyReceipt(auth, feeInWei, rlpHeader, noOfConfirmations,
				rlpEncodedValue, path, rlpEncodedProofNodes)
		case VALUE_TYPE_STATE:
			tx, err = c.chains[chain].testimoniumContract.VerifyState(auth, feeInWei, rlpHeader, noOfConfirmations,
				rlpEncodedValue, path, rlpEncodedProofNodes)
		default:
			log.Fatal("Unexpected trie value type: ", trieValueType)
	}

	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	receipt, err := awaitTxReceipt(c.chains[chain].client, tx.Hash())
	if err != nil {
		log.Fatal(err)
	}

	if receipt.Status == 0 {
		// Transaction failed
		reason := getFailureReason(c.chains[chain].client, c.account, tx, receipt.BlockNumber)
		fmt.Printf("Tx failed: %s\n", reason)
		return
	}

	var verificationResult *VerificationResult

	switch trieValueType {
		case VALUE_TYPE_TRANSACTION:
			verificationResult, err = c.getVerifyTransactionEvent(chain, receipt)
		case VALUE_TYPE_RECEIPT:
			verificationResult, err = c.getVerifyReceiptEvent(chain, receipt)
		case VALUE_TYPE_STATE:
			verificationResult, err = c.getVerifyStateEvent(chain, receipt)
	}

	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Tx successful: %s\n", verificationResult.String())
}

func (c Client) getVerifyTransactionEvent(chain uint8, receipt *types.Receipt) (*VerificationResult, error) {
	eventIterator, err := c.chains[chain].testimoniumContract.TestimoniumFilterer.FilterVerifyTransaction(
		&bind.FilterOpts{
			Start:   receipt.BlockNumber.Uint64(),
			End:     nil,
			Context: nil,
		})
	if err != nil {
		return nil, err
	}
	if eventIterator.Next() {
		return &VerificationResult{
			returnCode: eventIterator.Event.Result,
		}, nil
	}
	return nil, fmt.Errorf("no event found")
}

func (c Client) getVerifyReceiptEvent(chain uint8, receipt *types.Receipt) (*VerificationResult, error) {
	eventIterator, err := c.chains[chain].testimoniumContract.TestimoniumFilterer.FilterVerifyReceipt(
		&bind.FilterOpts{
			Start:   receipt.BlockNumber.Uint64(),
			End:     nil,
			Context: nil,
		})
	if err != nil {
		return nil, err
	}
	if eventIterator.Next() {
		return &VerificationResult{
			returnCode: eventIterator.Event.Result,
		}, nil
	}
	return nil, fmt.Errorf("no event found")
}

func (c Client) getVerifyStateEvent(chain uint8, receipt *types.Receipt) (*VerificationResult, error) {
	eventIterator, err := c.chains[chain].testimoniumContract.TestimoniumFilterer.FilterVerifyState(
		&bind.FilterOpts{
			Start:   receipt.BlockNumber.Uint64(),
			End:     nil,
			Context: nil,
		})
	if err != nil {
		return nil, err
	}
	if eventIterator.Next() {
		return &VerificationResult{
			returnCode: eventIterator.Event.Result,
		}, nil
	}
	return nil, fmt.Errorf("no event found")
}

func (c Client) SetEpochData(epochData typedefs.EpochData, chain uint8) {
	if _, exists := c.chains[chain]; !exists {
		log.Fatalf("Chain '%d' does not exist", chain)
	}

	nodes := []*big.Int{}
	start := big.NewInt(0)
	//fmt.Printf("No meaningful nodes: %d\n", len(epochData.MerkleNodes))
	for k, n := range epochData.MerkleNodes {
		nodes = append(nodes, n)
		if len(nodes) == 40 || k == len(epochData.MerkleNodes)-1 {
			mnlen := big.NewInt(int64(len(nodes)))
			fmt.Printf("Going to do tx\n")

			if k < 440 && epochData.Epoch.Uint64() == 128 {
				start.Add(start, mnlen)
				nodes = []*big.Int{}
				continue
			}

			auth := prepareTransaction(c.account, c.privateKey, c.chains[chain], big.NewInt(0))

			tx, err := c.chains[chain].ethashContract.SetEpochData(auth, epochData.Epoch, epochData.FullSizeIn128Resolution,
				epochData.BranchDepth, nodes, start, mnlen)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

			receipt, err := awaitTxReceipt(c.chains[chain].client, tx.Hash())
			if err != nil {
				log.Fatal(err)
			}
			if receipt.Status == 0 {
				// Transaction failed
				reason := getFailureReason(c.chains[chain].client, c.account, tx, receipt.BlockNumber)
				fmt.Printf("Tx failed: %s\n", reason)
				return
			}

			start.Add(start, mnlen)
			nodes = []*big.Int{}
		}
	}
}

func (c Client) DeployTestimonium(targetChain uint8, sourceChain uint8, genesisBlockNumber uint64) (common.Address) {
	if _, exists := c.chains[targetChain]; !exists {
		log.Fatalf("Target chain '%d' does not exist", targetChain)
	}
	if _, exists := c.chains[sourceChain]; !exists {
		log.Fatalf("Source chain '%d' does not exist", sourceChain)
	}

	header, err := c.HeaderByNumber(new(big.Int).SetUint64(genesisBlockNumber), sourceChain)
	if err != nil {
		log.Fatal("Failed to retrieve header from source chain: " + err.Error())
	}

	totalDifficulty, err := c.TotalDifficulty(new(big.Int).SetUint64(genesisBlockNumber), sourceChain)
	if err != nil {
		log.Fatalf("Failed to retrieve total difficulty of block %d: %s", genesisBlockNumber, err)
	}

	rlpHeader, err := encodeHeaderToRLP(header)
	if err != nil {
		log.Fatal("Failed to encode header to RLP: " + err.Error())
	}

	auth := prepareTransaction(c.account, c.privateKey, c.chains[targetChain], big.NewInt(0))

	addr, tx, _, err := DeployTestimonium(auth, c.chains[targetChain].client, rlpHeader, totalDifficulty, c.chains[targetChain].ethashContractAddress)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	receipt, err := awaitTxReceipt(c.chains[targetChain].client, tx.Hash())
	if err != nil {
		log.Fatal(err)
	}
	if receipt.Status == 0 {
		// Transaction failed
		reason := getFailureReason(c.chains[targetChain].client, c.account, tx, receipt.BlockNumber)
		fmt.Printf("Tx failed: %s\n", reason)
		return common.Address{}
	}

	fmt.Println("Contract has been deployed at address: ", addr.String())
	return addr
}

func (c Client) DeployEthash(targetChain uint8) (common.Address) {
	if _, exists := c.chains[targetChain]; !exists {
		log.Fatalf("Target chain '%d' does not exist", targetChain)
	}

	auth := prepareTransaction(c.account, c.privateKey, c.chains[targetChain], big.NewInt(0))

	addr, tx, _, err := ethash.DeployEthash(auth, c.chains[targetChain].client)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Tx submitted: %s\n", tx.Hash().Hex())

	receipt, err := awaitTxReceipt(c.chains[targetChain].client, tx.Hash())
	if err != nil {
		log.Fatal(err)
	}

	if receipt.Status == 0 {
		// Transaction failed
		reason := getFailureReason(c.chains[targetChain].client, c.account, tx, receipt.BlockNumber)
		fmt.Printf("Tx failed: %s\n", reason)
		return common.Address{}
	}

	fmt.Println("Contract has been deployed at address: ", addr.String())

	return addr
}

func getFailureReason(client *ethclient.Client, from common.Address, tx *types.Transaction, blockNumber *big.Int) string {
	code, err := client.CallContract(context.Background(), createCallMsgFromTransaction(from, tx), blockNumber)

	if err != nil {
		log.Fatal(err)
	}

	return fmt.Sprintf(string(code[67:]))
}

func createCallMsgFromTransaction(from common.Address, tx *types.Transaction) ethereum.CallMsg {
	return ethereum.CallMsg{
		From:     from,
		To:       tx.To(),
		Gas:      tx.Gas(),
		GasPrice: tx.GasPrice(),
		Value:    tx.Value(),
		Data:     tx.Data(),
	}
}

func encodeHeaderToRLP(header *types.Header) ([]byte, error) {
	buffer := new(bytes.Buffer)

	err := rlp.Encode(buffer, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
		header.MixDigest,
		header.Nonce,
	})

	// be careful when passing byte-array as buffer, the pointer can change if the buffer is used again
	return buffer.Bytes(), err
}

func decodeHeaderFromRLP(bytes []byte) (*types.Header, error) {
	header := new(types.Header)

	err := rlp.DecodeBytes(bytes, header)

	return header, err
}

func encodeHeaderWithoutNonceToRLP(header *types.Header) ([]byte, error) {
	buffer := new(bytes.Buffer)

	err := rlp.Encode(buffer, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
	})

	return buffer.Bytes(), err
}

func prepareTransaction(from common.Address, privateKey *ecdsa.PrivateKey, chain *Chain, valueInWei *big.Int) *bind.TransactOpts {
	nonce, err := chain.client.PendingNonceAt(context.Background(), from)
	if err != nil {
		log.Fatal(err)
	}

	gasPrice, err := chain.client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	auth := bind.NewKeyedTransactor(privateKey)
	auth.From = from
	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = valueInWei // in wei
	auth.GasPrice = gasPrice

	// one could also set the gas limit, however it seems that the right gas limit is only estimated
	// if the gas limit is not set specifically
	return auth
}

func awaitTxReceipt(client *ethclient.Client, txHash common.Hash) (*types.Receipt, error) {
	const TimeoutLength = 2
	receipts := make(chan *types.Receipt)

	go func(chan *types.Receipt) {
		for ; ; {
			receipt, _ := client.TransactionReceipt(context.Background(), txHash)

			if receipt != nil {
				receipts <- receipt
			}
		}
	}(receipts)

	select {
	case receipt := <-receipts:
		return receipt, nil
	case <-time.After(TimeoutLength * time.Minute):
		return nil, fmt.Errorf("timeout: did not receive receipt after %d minutes", TimeoutLength)
	}

	//query := ethereum.FilterQuery{
	//	Addresses: []common.Address{chain.testimoniumContractAddress},
	//}
	//
	//logs := make(chan types.Log)
	//
	//sub, err := chain.client.SubscribeFilterLogs(context.Background(), query, logs)
	//if err != nil {
	//	log.Fatal(err)
	//}
	//
	//for {
	//	select {
	//	case err := <-sub.Err():
	//		return nil, err
	//	case vLog := <-logs:
	//		// if the transaction hash of the event does not equal the passed
	//		// transaction hash we continue listening
	//		if vLog.TxHash.Hex() != txHash.Hex() {
	//			break
	//		}
	//		return parseEvent(vLog)
	//	}
	//}
}
