package framework

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"google.golang.org/grpc/credentials/insecure"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ibftOp "github.com/0xPolygon/polygon-edge/consensus/ibft/proto"

	"github.com/0xPolygon/polygon-edge/command/genesis"
	"github.com/0xPolygon/polygon-edge/command/helper"
	secretsCommand "github.com/0xPolygon/polygon-edge/command/secrets"
	"github.com/0xPolygon/polygon-edge/command/server"
	"github.com/0xPolygon/polygon-edge/crypto"
	"github.com/0xPolygon/polygon-edge/helper/tests"
	"github.com/0xPolygon/polygon-edge/network"
	"github.com/0xPolygon/polygon-edge/secrets"
	"github.com/0xPolygon/polygon-edge/secrets/local"
	"github.com/0xPolygon/polygon-edge/server/proto"
	txpoolProto "github.com/0xPolygon/polygon-edge/txpool/proto"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/umbracle/go-web3"
	"github.com/umbracle/go-web3/jsonrpc"
	"google.golang.org/grpc"
	empty "google.golang.org/protobuf/types/known/emptypb"
)

type TestServerConfigCallback func(*TestServerConfig)

const (
	initialPort = 12000
	binaryName  = "polygon-edge"
)

type TestServer struct {
	t *testing.T

	Config *TestServerConfig
	cmd    *exec.Cmd
}

func NewTestServer(t *testing.T, rootDir string, callback TestServerConfigCallback) *TestServer {
	t.Helper()

	// Reserve ports
	ports, err := FindAvailablePorts(3, initialPort, initialPort+10000)
	if err != nil {
		t.Fatal(err)
	}

	// Sets the services to start on open ports
	config := &TestServerConfig{
		ReservedPorts: ports,
		GRPCPort:      ports[0].Port(),
		LibP2PPort:    ports[1].Port(),
		JSONRPCPort:   ports[2].Port(),
		RootDir:       rootDir,
	}

	if callback != nil {
		callback(config)
	}

	return &TestServer{
		t:      t,
		Config: config,
	}
}

func (t *TestServer) GrpcAddr() string {
	return fmt.Sprintf("http://127.0.0.1:%d", t.Config.GRPCPort)
}

func (t *TestServer) JSONRPCAddr() string {
	return fmt.Sprintf("http://127.0.0.1:%d", t.Config.JSONRPCPort)
}

func (t *TestServer) JSONRPC() *jsonrpc.Client {
	clt, err := jsonrpc.NewClient(t.JSONRPCAddr())
	if err != nil {
		t.t.Fatal(err)
	}

	return clt
}

func (t *TestServer) Operator() proto.SystemClient {
	conn, err := grpc.Dial(
		fmt.Sprintf("127.0.0.1:%d", t.Config.GRPCPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.t.Fatal(err)
	}

	return proto.NewSystemClient(conn)
}

func (t *TestServer) TxnPoolOperator() txpoolProto.TxnPoolOperatorClient {
	conn, err := grpc.Dial(
		fmt.Sprintf("127.0.0.1:%d", t.Config.GRPCPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.t.Fatal(err)
	}

	return txpoolProto.NewTxnPoolOperatorClient(conn)
}

func (t *TestServer) IBFTOperator() ibftOp.IbftOperatorClient {
	conn, err := grpc.Dial(
		fmt.Sprintf("127.0.0.1:%d", t.Config.GRPCPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.t.Fatal(err)
	}

	return ibftOp.NewIbftOperatorClient(conn)
}

func (t *TestServer) ReleaseReservedPorts() {
	for _, p := range t.Config.ReservedPorts {
		if err := p.Close(); err != nil {
			t.t.Error(err)
		}
	}

	t.Config.ReservedPorts = nil
}

func (t *TestServer) Stop() {
	t.ReleaseReservedPorts()

	if t.cmd != nil {
		if err := t.cmd.Process.Kill(); err != nil {
			t.t.Error(err)
		}
	}
}

func (t *TestServer) GetLatestBlockHeight() (uint64, error) {
	return t.JSONRPC().Eth().BlockNumber()
}

type InitIBFTResult struct {
	Address string
	NodeID  string
}

func (t *TestServer) InitIBFT() (*InitIBFTResult, error) {
	secretsInitCmd := secretsCommand.SecretsInit{}

	var args []string

	commandSlice := strings.Split(secretsInitCmd.GetBaseCommand(), " ")
	args = append(args, commandSlice...)
	args = append(args, "--data-dir", t.Config.IBFTDir)

	cmd := exec.Command(binaryName, args...)
	cmd.Dir = t.Config.RootDir

	if _, err := cmd.Output(); err != nil {
		return nil, err
	}

	res := &InitIBFTResult{}

	localSecretsManager, factoryErr := local.SecretsManagerFactory(
		nil,
		&secrets.SecretsManagerParams{
			Logger: hclog.NewNullLogger(),
			Extra: map[string]interface{}{
				secrets.Path: filepath.Join(cmd.Dir, t.Config.IBFTDir),
			},
		})
	if factoryErr != nil {
		return nil, factoryErr
	}

	// Generate the IBFT validator private key
	validatorKey, validatorKeyEncoded, keyErr := crypto.GenerateAndEncodePrivateKey()
	if keyErr != nil {
		return nil, keyErr
	}

	// Write the validator private key to the secrets manager storage
	if setErr := localSecretsManager.SetSecret(secrets.ValidatorKey, validatorKeyEncoded); setErr != nil {
		return nil, setErr
	}

	// Generate the libp2p private key
	libp2pKey, libp2pKeyEncoded, keyErr := network.GenerateAndEncodeLibp2pKey()
	if keyErr != nil {
		return nil, keyErr
	}

	// Write the networking private key to the secrets manager storage
	if setErr := localSecretsManager.SetSecret(secrets.NetworkKey, libp2pKeyEncoded); setErr != nil {
		return nil, setErr
	}

	// Get the node ID from the private key
	nodeID, err := peer.IDFromPrivateKey(libp2pKey)
	if err != nil {
		return nil, err
	}

	res.Address = crypto.PubKeyToAddress(&validatorKey.PublicKey).String()
	res.NodeID = nodeID.String()

	return res, nil
}

func (t *TestServer) GenerateGenesis() error {
	genesisCmd := genesis.GenesisCommand{}
	args := []string{
		genesisCmd.GetBaseCommand(),
	}

	// add pre-mined accounts
	for _, acct := range t.Config.PremineAccts {
		args = append(args, "--premine", acct.Addr.String()+":0x"+acct.Balance.Text(16))
	}

	// add consensus flags
	switch t.Config.Consensus {
	case ConsensusIBFT:
		args = append(args, "--consensus", "ibft")

		if t.Config.IBFTDirPrefix == "" {
			return errors.New("prefix of IBFT directory is not set")
		}

		args = append(args, "--ibft-validators-prefix-path", t.Config.IBFTDirPrefix)

		if t.Config.EpochSize != 0 {
			args = append(args, "--epoch-size", strconv.FormatUint(t.Config.EpochSize, 10))
		}
	case ConsensusDev:
		args = append(args, "--consensus", "dev")

		// Set up any initial staker addresses for the predeployed Staking SC
		for _, stakerAddress := range t.Config.DevStakers {
			args = append(args, "--ibft-validator", stakerAddress.String())
		}
	case ConsensusDummy:
		args = append(args, "--consensus", "dummy")
	}

	for _, bootnode := range t.Config.Bootnodes {
		args = append(args, "--bootnode", bootnode)
	}

	// Make sure the correct mechanism is selected
	if t.Config.IsPos {
		args = append(args, "--pos")
	}

	// add block gas limit
	if t.Config.BlockGasLimit == 0 {
		t.Config.BlockGasLimit = helper.GenesisGasLimit
	}

	blockGasLimit := strconv.FormatUint(t.Config.BlockGasLimit, 10)
	args = append(args, "--block-gas-limit", blockGasLimit)

	cmd := exec.Command(binaryName, args...)
	cmd.Dir = t.Config.RootDir

	return cmd.Run()
}

func (t *TestServer) Start(ctx context.Context) error {
	serverCmd := server.ServerCommand{}
	args := []string{
		serverCmd.GetBaseCommand(),
		// add custom chain
		"--chain", filepath.Join(t.Config.RootDir, "genesis.json"),
		// enable grpc
		"--grpc", fmt.Sprintf(":%d", t.Config.GRPCPort),
		// enable libp2p
		"--libp2p", fmt.Sprintf(":%d", t.Config.LibP2PPort),
		// enable jsonrpc
		"--jsonrpc", fmt.Sprintf(":%d", t.Config.JSONRPCPort),
	}

	switch t.Config.Consensus {
	case ConsensusIBFT:
		args = append(args, "--data-dir", filepath.Join(t.Config.RootDir, t.Config.IBFTDir))
	case ConsensusDev:
		args = append(args, "--data-dir", t.Config.RootDir)
		args = append(args, "--dev")

		if t.Config.DevInterval != 0 {
			args = append(args, "--dev-interval", strconv.Itoa(t.Config.DevInterval))
		}
	case ConsensusDummy:
		args = append(args, "--data-dir", t.Config.RootDir)
	}

	if t.Config.Seal {
		args = append(args, "--seal")
	}

	if t.Config.PriceLimit != nil {
		args = append(args, "--price-limit", strconv.FormatUint(*t.Config.PriceLimit, 10))
	}

	if t.Config.ShowsLog {
		args = append(args, "--log-level", "debug")
	}

	// add block gas target
	if t.Config.BlockGasTarget != 0 {
		args = append(args, "--block-gas-target", *types.EncodeUint64(t.Config.BlockGasTarget))
	}

	t.ReleaseReservedPorts()

	// Start the server
	t.cmd = exec.Command(binaryName, args...)
	t.cmd.Dir = t.Config.RootDir

	if t.Config.ShowsLog {
		stdout := io.Writer(os.Stdout)
		t.cmd.Stdout = stdout
		t.cmd.Stderr = stdout
	}

	if err := t.cmd.Start(); err != nil {
		return err
	}

	_, err := tests.RetryUntilTimeout(ctx, func() (interface{}, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if _, err := t.Operator().GetStatus(ctx, &empty.Empty{}); err == nil {
			return nil, false
		}

		return nil, true
	})

	return err
}

// DeployContract deploys a contract with account 0 and returns the address
func (t *TestServer) DeployContract(ctx context.Context, binary string) (web3.Address, error) {
	buf, err := hex.DecodeString(binary)
	if err != nil {
		return web3.Address{}, err
	}

	receipt, err := t.SendTxn(ctx, &web3.Transaction{
		Input: buf,
	})

	if err != nil {
		return web3.Address{}, err
	}

	return receipt.ContractAddress, nil
}

const (
	DefaultGasPrice = 1879048192 // 0x70000000
	DefaultGasLimit = 5242880    // 0x500000
)

var emptyAddr web3.Address

func (t *TestServer) SendTxn(ctx context.Context, txn *web3.Transaction) (*web3.Receipt, error) {
	client := t.JSONRPC()

	if txn.From == emptyAddr {
		txn.From = web3.Address(t.Config.PremineAccts[0].Addr)
	}

	if txn.GasPrice == 0 {
		txn.GasPrice = DefaultGasPrice
	}

	if txn.Gas == 0 {
		txn.Gas = DefaultGasLimit
	}

	hash, err := client.Eth().SendTransaction(txn)
	if err != nil {
		return nil, err
	}

	return tests.WaitForReceipt(ctx, t.JSONRPC().Eth(), hash)
}

type PreparedTransaction struct {
	From     types.Address
	GasPrice *big.Int
	Gas      uint64
	To       *types.Address
	Value    *big.Int
	Input    []byte
}

// SendRawTx signs the transaction with the provided private key, executes it, and returns the receipt
func (t *TestServer) SendRawTx(
	ctx context.Context,
	tx *PreparedTransaction,
	signerKey *ecdsa.PrivateKey,
) (*web3.Receipt, error) {
	signer := crypto.NewEIP155Signer(100)
	client := t.JSONRPC()

	nextNonce, err := client.Eth().GetNonce(web3.Address(tx.From), web3.Latest)
	if err != nil {
		return nil, err
	}

	signedTx, err := signer.SignTx(&types.Transaction{
		From:     tx.From,
		GasPrice: tx.GasPrice,
		Gas:      tx.Gas,
		To:       tx.To,
		Value:    tx.Value,
		Input:    tx.Input,
		Nonce:    nextNonce,
	}, signerKey)
	if err != nil {
		return nil, err
	}

	txHash, err := client.Eth().SendRawTransaction(signedTx.MarshalRLP())
	if err != nil {
		return nil, err
	}

	return tests.WaitForReceipt(ctx, t.JSONRPC().Eth(), txHash)
}

func (t *TestServer) WaitForReceipt(ctx context.Context, hash web3.Hash) (*web3.Receipt, error) {
	client := t.JSONRPC()

	type result struct {
		receipt *web3.Receipt
		err     error
	}

	res, err := tests.RetryUntilTimeout(ctx, func() (interface{}, bool) {
		receipt, err := client.Eth().GetTransactionReceipt(hash)
		if err != nil && err.Error() != "not found" {
			return result{receipt, err}, false
		}
		if receipt != nil {
			return result{receipt, nil}, false
		}

		return nil, true
	})
	if err != nil {
		return nil, err
	}

	data, ok := res.(result)
	if !ok {
		return nil, errors.New("invalid type assertion")
	}

	return data.receipt, data.err
}

func (t *TestServer) WaitForReady(ctx context.Context) error {
	_, err := tests.RetryUntilTimeout(ctx, func() (interface{}, bool) {
		num, err := t.GetLatestBlockHeight()
		if err != nil {
			return nil, true
		}
		if num == 0 {
			return nil, true
		}

		return num, false
	})

	return err
}

func (t *TestServer) TxnTo(ctx context.Context, address web3.Address, method string) *web3.Receipt {
	sig := MethodSig(method)
	receipt, err := t.SendTxn(ctx, &web3.Transaction{
		To:    &address,
		Input: sig,
	})

	if err != nil {
		t.t.Fatal(err)
	}

	return receipt
}
