package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/fox-one/mixin-sdk-go/v2"
	"github.com/fox-one/mixin-sdk-go/v2/mixinnet"
	"github.com/manifoldco/promptui"
	"github.com/shopspring/decimal"
	"github.com/spf13/cast"
)

var (
	keystorePath    = flag.String("key", "", "keystore path")
	spendGroupCount = flag.Int("group", 256, "spend group count")
)

func main() {
	ctx := context.Background()

	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		log.Fatalln("receiver id is required")
	}

	if *spendGroupCount <= 0 || *spendGroupCount > 256 {
		log.Fatalln("invalid spend group count")
	}

	key, err := loadKeystore(*keystorePath)
	if err != nil {
		log.Fatalln("load keystore failed:", err)
	}

	client, err := mixin.NewFromKeystore(&key.Keystore)
	if err != nil {
		log.Fatalln("new client failed:", err)
	}

	receiver, err := fetchUserInfo(ctx, args[0])
	if err != nil {
		log.Fatalln("read receiver info failed:", err)
	}

	if cast.ToInt64(receiver.IdentityNumber) == 0 {
		log.Fatalln("receiver is not a mixin messenger user")
	}

	if receiver.UserID == client.ClientID {
		log.Fatalln("receiver is self")
	}

	log.Printf("migrate assets to %s(%s)", receiver.FullName, receiver.UserID)
	if !conformContinue() {
		return
	}

	r := &runner{
		client:   client,
		key:      key,
		output:   *keystorePath,
		receiver: receiver.UserID,
	}

	if err := r.migrateLegacyAssets(ctx); err != nil {
		log.Fatalln("migrate legacy assets failed:", err)
	}

	if err := r.updateTipPin(ctx); err != nil {
		log.Fatalln("update tip pin failed:", err)
	}

	if err := r.migrateToSafe(ctx); err != nil {
		log.Fatalln("migrate safe failed:", err)
	}

	if err := r.migrateSafeAssets(ctx); err != nil {
		log.Fatalln("migrate safe assets failed:", err)
	}
}

type runner struct {
	client   *mixin.Client
	key      *Keystore
	output   string
	receiver string
}

func (r *runner) saveKeystore() error {
	f, err := os.OpenFile(r.output, os.O_WRONLY, 0777)
	if err != nil {
		return fmt.Errorf("open keystore failed: %w", err)
	}

	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r.key); err != nil {
		return fmt.Errorf("save keystore failed: %w", err)
	}

	return nil
}

func (r *runner) migrateLegacyAssets(ctx context.Context) error {
	assets, err := r.client.ReadAssets(ctx)
	if err != nil {
		return fmt.Errorf("list legacy assets failed: %w", err)
	}

	var idx int
	for _, asset := range assets {
		if asset.Balance.IsZero() {
			continue
		}

		assets[idx] = asset
		idx++
	}

	assets = assets[:idx]
	if len(assets) == 0 {
		log.Println("no legacy assets")
		return nil
	}

	log.Printf("start migrating %d legacy assets\n", len(assets))

	for _, asset := range assets {
		log.Println("migrating legacy asset", asset.Balance, asset.Symbol)
		t := &mixin.TransferInput{
			AssetID:    asset.AssetID,
			OpponentID: r.receiver,
			Amount:     asset.Balance,
			TraceID:    mixin.RandomTraceID(),
			Memo:       "migrate by mixin-migrate",
		}

		snapshot, err := r.client.Transfer(ctx, t, r.key.Pin)
		if err != nil {
			return fmt.Errorf("transfer %s %s failed: %w", asset.Balance, asset.Symbol, err)
		}

		log.Println("migrated legacy asset", asset.Balance, asset.Symbol, "snapshot", snapshot.SnapshotID)
	}

	log.Printf("migrate legacy assets done\n")
	return nil
}

func (r *runner) updateTipPin(ctx context.Context) error {
	if _, err := mixinnet.KeyFromString(r.key.Pin); err == nil {
		log.Println("updated to tip pin already")
		return nil
	}

	log.Println("start update tip pin")

	key := mixinnet.GenerateKey(rand.Reader)
	log.Println("new pin:", key.String())

	if err := r.client.ModifyPin(ctx, r.key.Pin, key.Public().String()); err != nil {
		return fmt.Errorf("modify pin failed: %w", err)
	}

	r.key.Pin = key.String()
	if err := r.saveKeystore(); err != nil {
		return err
	}

	log.Println("update tip pin done")
	return nil
}

func (r *runner) migrateToSafe(ctx context.Context) error {
	if _, err := mixinnet.KeyFromString(r.key.SpendKey); err == nil {
		log.Println("migrated to safe already")
		return nil
	}

	log.Println("start migrate safe")

	newSpendKey := mixinnet.GenerateKey(rand.Reader).String()
	log.Println("new spend key:", newSpendKey)

	if _, err := r.client.SafeMigrate(ctx, newSpendKey, r.key.Pin); err != nil {
		return fmt.Errorf("migrate safe failed: %w", err)
	}

	r.key.SpendKey = newSpendKey
	if err := r.saveKeystore(); err != nil {
		return err
	}

	log.Println("migrate safe done")
	return nil
}

func (r *runner) migrateSafeAssets(ctx context.Context) error {
	assets := map[string][]*mixin.SafeUtxo{}

	opt := mixin.SafeListUtxoOption{
		Offset: 0,
		Limit:  500,
		Order:  "ASC",
		State:  mixin.SafeUtxoStateUnspent,
	}

	for {
		utxos, err := r.client.SafeListUtxos(ctx, opt)
		if err != nil {
			return fmt.Errorf("list safe utxos failed: %w", err)
		}

		if len(utxos) == 0 {
			break
		}

		for _, utxo := range utxos {
			opt.Offset = utxo.Sequence + 1
			assets[utxo.AssetID] = append(assets[utxo.AssetID], utxo)
		}
	}

	if len(assets) == 0 {
		log.Println("no safe assets")
		return nil
	}

	log.Printf("start migrating %d safe assets\n", len(assets))

	spendKey, err := mixinnet.KeyFromString(r.key.SpendKey)
	if err != nil {
		return fmt.Errorf("invalid spend key: %w", err)
	}

	for assetId, utxos := range assets {
		asset, err := r.client.SafeReadAsset(ctx, assetId)
		if err != nil {
			return fmt.Errorf("read safe asset %s failed: %w", assetId, err)
		}

		log.Println("migrating safe asset", sumUtxos(utxos), asset.Symbol)

		for idx := 0; idx < len(utxos); idx += *spendGroupCount {
			spends := utxos[idx:min(len(utxos), idx+*spendGroupCount)]
			b := mixin.NewSafeTransactionBuilder(spends)
			b.Memo = "migrate by mixin-migrate"

			output := mixin.TransactionOutput{
				Address: mixin.RequireNewMixAddress([]string{r.receiver}, 1),
				Amount:  sumUtxos(spends),
			}

			tx, err := r.client.MakeTransaction(ctx, b, []*mixin.TransactionOutput{&output})
			if err != nil {
				return fmt.Errorf("make safe transaction failed: %w", err)
			}

			raw, err := tx.Dump()
			if err != nil {
				return fmt.Errorf("dump transaction failed: %w", err)
			}

			req, err := r.client.SafeCreateTransactionRequest(ctx, &mixin.SafeTransactionRequestInput{
				RequestID:      b.Hint,
				RawTransaction: raw,
			})

			if err != nil {
				return fmt.Errorf("create transaction request failed: %w", err)
			}

			if err := mixin.SafeSignTransaction(tx, spendKey, req.Views, 0); err != nil {
				return fmt.Errorf("sign transaction failed: %w", err)
			}

			signedRaw, err := tx.Dump()
			if err != nil {
				return fmt.Errorf("dump transaction failed: %w", err)
			}

			if _, err := r.client.SafeSubmitTransactionRequest(ctx, &mixin.SafeTransactionRequestInput{
				RequestID:      req.RequestID,
				RawTransaction: signedRaw,
			}); err != nil {
				return fmt.Errorf("submit transaction request failed: %w", err)
			}
		}

		log.Println("migrated safe asset", sumUtxos(utxos), asset.Symbol)
	}

	log.Println("migrate safe assets done")
	return nil
}

func sumUtxos(utxos []*mixin.SafeUtxo) decimal.Decimal {
	var sum decimal.Decimal
	for _, utxo := range utxos {
		sum = sum.Add(utxo.Amount)
	}
	return sum
}

func fetchUserInfo(ctx context.Context, id string) (*mixin.User, error) {
	uri := fmt.Sprintf("https://echo.yiplee.com/users/%s", id)
	resp, err := http.Get(uri)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var body struct {
		Data mixin.User `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	return &body.Data, nil
}

func conformContinue() bool {
	prompt := promptui.Prompt{
		Label:     "Continue",
		IsConfirm: true,
	}
	result, err := prompt.Run()
	if err != nil {
		return false
	}

	return strings.EqualFold(result, "y")
}
