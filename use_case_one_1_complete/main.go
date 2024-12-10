package main

import (
	"C"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goombaio/namegenerator"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func createClient(c string, u string, p string, caFile string) (*mongo.Client, error) {
	//auth setup
	creds := options.Credential{
		Username:      u,
		Password:      p,
		AuthMechanism: "SCRAM-SHA-256",
	}

	// TLS setup
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("failed to append CA certificate")
	}

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	// instantiate client
	opts := options.Client().ApplyURI(c).SetAuth(creds).SetTLSConfig(tlsConfig)
	client, err := mongo.Connect(context.TODO(), opts)
	if err != nil {
		return nil, err
	}
	err = client.Ping(context.Background(), nil)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func createAutoEncryptionClient(c string, u string, p string, caFile string, ns string, kms map[string]map[string]interface{}, tlsOps map[string]*tls.Config, s bson.M) (*mongo.Client, error) { //auth setup
	creds := options.Credential{
		Username:      u,
		Password:      p,
		AuthMechanism: "SCRAM-SHA-256",
	}

	// TLS setup
	caCert, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("failed to append CA certificate")
	}

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	autoEncryptionOpts := options.AutoEncryption().
		SetKeyVaultNamespace(ns).
		SetKmsProviders(kms).
		SetSchemaMap(s).
		SetTLSConfig(tlsOps)

	client, err := mongo.Connect(
		context.TODO(),
		options.Client().ApplyURI(c).SetAutoEncryptionOptions(autoEncryptionOpts).SetAuth(creds).SetTLSConfig(tlsConfig),
	)

	if err != nil {
		return nil, err
	}

	return client, nil
}

func createManualEncryptionClient(c *mongo.Client, kp map[string]map[string]interface{}, kns string, tlsOps map[string]*tls.Config) (*mongo.ClientEncryption, error) {
	o := options.ClientEncryption().SetKeyVaultNamespace(kns).SetKmsProviders(kp).SetTLSConfig(tlsOps)
	client, err := mongo.NewClientEncryption(c, o)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func encryptManual(ce *mongo.ClientEncryption, dek primitive.Binary, alg string, data interface{}) (primitive.Binary, error) {
	var out primitive.Binary
	rawValueType, rawValueData, err := bson.MarshalValue(data)
	if err != nil {
		return primitive.Binary{}, err
	}

	rawValue := bson.RawValue{Type: rawValueType, Value: rawValueData}

	encryptionOpts := options.Encrypt().
		SetAlgorithm(alg).
		SetKeyID(dek)

	out, err = ce.Encrypt(context.TODO(), rawValue, encryptionOpts)
	if err != nil {
		return primitive.Binary{}, err
	}

	return out, nil
}

func createDEK(c *mongo.ClientEncryption, kn string, cmk map[string]interface{}, altName string) (primitive.Binary, error) {
	var (
		dek primitive.Binary
		err error
	)

	cOpts := options.DataKey().
		SetMasterKey(cmk).
		SetKeyAltNames([]string{altName})
	dek, err = c.CreateDataKey(context.TODO(), kn, cOpts)
	if err != nil {
		return primitive.Binary{}, err
	}

	return dek, nil
}

func getDEK(c *mongo.ClientEncryption, altName string) (primitive.Binary, error) {
	var dekFindResult bson.M

	err := c.GetKeyByAltName(context.TODO(), altName).Decode(&dekFindResult)
	if err != nil {
		return primitive.Binary{}, err
	}
	if len(dekFindResult) == 0 {
		return primitive.Binary{}, nil
	}
	b, ok := dekFindResult["_id"].(primitive.Binary)
	if !ok {
		return primitive.Binary{}, errors.New("the DEK conversion error")
	}
	return b, nil
}

func nameGenerator() (string, string) {
	seed := time.Now().UTC().UnixNano()
	nameGenerator := namegenerator.NewNameGenerator(seed)

	name := nameGenerator.Generate()

	firstName := strings.Split(name, "-")[0]
	lastName := strings.Split(name, "-")[1]

	return firstName, lastName
}

func main() {
	var (
		caFile           = "/data/pki/ca.pem"
		username         = "app_user"
		password         = "SuperP@ssword123!"
		client           *mongo.Client
		encryptedClient  *mongo.Client
		clientEncryption *mongo.ClientEncryption
		connectionString = "mongodb://mongodb-0:27017/?replicaSet=rs0&tls=true"
		employeeDEK      primitive.Binary
		err              error
		exitCode         = 0
		findResult       bson.M
		keyVaultColl     = "__keyVault"
		keyVaultDB       = "__encryption"

		kmipTLSConfig *tls.Config
		result        *mongo.InsertOneResult
	)

	defer func() {
		os.Exit(exitCode)
	}()

	provider := "kmip"
	kmsProvider := map[string]map[string]interface{}{
		provider: {
			"endpoint": "kmip-0:5696",
		},
	}
	cmk := map[string]interface{}{
		"keyId": "1", // this is our CMK ID
	}
	keySpace := keyVaultDB + "." + keyVaultColl

	client, err = createClient(connectionString, username, password, caFile)
	if err != nil {
		fmt.Printf("MDB client error: %s\n", err)
		exitCode = 1
		return
	}

	// Set the KMIP TLS options
	kmsTLSOptions := make(map[string]*tls.Config)
	tlsOptions := map[string]interface{}{
		"tlsCAFile":             "/data/pki/ca.pem",
		"tlsCertificateKeyFile": "/data/pki/client-0.pem",
	}
	kmipTLSConfig, err = options.BuildTLSConfig(tlsOptions)
	if err != nil {
		fmt.Printf("Cannot create KMS TLS Config: %s\n", err)
		exitCode = 1
		return
	}
	kmsTLSOptions["kmip"] = kmipTLSConfig

	clientEncryption, err = createManualEncryptionClient(client, kmsProvider, keySpace, kmsTLSOptions)
	if err != nil {
		fmt.Printf("ClientEncrypt error: %s\n", err)
		exitCode = 1
		return
	}

	rand.Seed(time.Now().UnixNano())
	id := strconv.Itoa(int(rand.Intn(100000)))

	// get our employee DEK or create
	employeeDEK, err = getDEK(clientEncryption, id)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			employeeDEK, err = createDEK(clientEncryption, provider, cmk, id)
			if err != nil {
				fmt.Printf("Cannot create employee DEK: %s\n", err)
				exitCode = 1
				return
			}
		} else {
			fmt.Printf("Cannot get employee DEK: %s\n", err)
			exitCode = 1
			return
		}
	}

	firstname, lastname := nameGenerator()
	payload := bson.M{
		"_id": id,
		"name": bson.M{
			"firstName":  firstname,
			"lastName":   lastname,
			"otherNames": nil,
		},
		"address": bson.M{
			"streetAddress": "29 Bson Street",
			"suburbCounty":  "Mongoville",
			"stateProvince": "Victoria",
			"zipPostcode":   "3999",
			"country":       "Oz",
		},
		"dob":           time.Date(1999, 1, 12, 0, 0, 0, 0, time.Local),
		"phoneNumber":   "1800MONGO",
		"salary":        999999.99,
		"taxIdentifier": "78SDSSWN001",
		"role":          []string{"Student"},
	}

	db := "companyData"
	collection := "employee"

	schemaMap := `{
  "bsonType": "object",
  "encryptMetadata": {
    "keyId": "/_id",
    "algorithm": "AEAD_AES_256_CBC_HMAC_SHA_512-Random"
  },
  "properties": {
    "name": {
      "bsonType": "object",
      "properties": {
        "otherNames": {
          "encrypt": {
            "bsonType": "string"
          }
        }
      }
    },
    "address": {
      "encrypt": {
        "bsonType": "object"
      }
    },
    "dob": {
      "encrypt": {
        "bsonType": "date"
      }
    },
    "phoneNumber": {
      "encrypt": {
        "bsonType": "string"
      }
    },
    "salary": {
      "encrypt": {
        "bsonType": "double"
      }
    },
    "taxIdentifier": {
      "encrypt": {
        "bsonType": "string"
      }
    }
  }
}`

	// Auto Encryption Client
	var testSchema bson.Raw
	err = bson.UnmarshalExtJSON([]byte(schemaMap), true, &testSchema)
	if err != nil {
		fmt.Printf("Unmarshal Error: %s\n", err)
	}
	completeMap := map[string]interface{}{
		"employData.employee": testSchema,
	}
	encryptedClient, err = createAutoEncryptionClient(connectionString, username, password, caFile, keySpace, kmsProvider, kmsTLSOptions, completeMap)
	if err != nil {
		fmt.Printf("MDB encrypted client error: %s\n", err)
		exitCode = 1
		return
	}

	encryptedColl := encryptedClient.Database(db).Collection(collection)

	// remove the otherNames field if it is nil
	name := payload["name"].(bson.M)
	if name["otherNames"] == nil {
		fmt.Println("Removing nil")
		delete(name, "otherNames")
	}
	// manually encrypt our firstName and lastName values:
	name["firstName"], err = encryptManual(clientEncryption, employeeDEK, "AEAD_AES_256_CBC_HMAC_SHA_512-Deterministic", name["firstName"])
	if err != nil {
		fmt.Printf("ClientEncrypt error: %s\n", err)
		exitCode = 1
		return
	}

	name["lastName"], err = encryptManual(clientEncryption, employeeDEK, "AEAD_AES_256_CBC_HMAC_SHA_512-Deterministic", name["lastName"])
	if err != nil {
		fmt.Printf("ClientEncrypt error: %s\n", err)
		exitCode = 1
		return
	}
	payload["name"] = name

	result, err = encryptedColl.InsertOne(context.TODO(), payload)
	if err != nil {
		fmt.Printf("Insert error: %s\n", err)
		exitCode = 1
		return
	}
	fmt.Println(result.InsertedID)

	err = encryptedColl.FindOne(context.TODO(), bson.M{"name.firstName": name["firstName"]}).Decode(&findResult)
	if err != nil {
		fmt.Printf("MongoDB find error: %s\n", err)
		exitCode = 1
		return
	}
	if len(findResult) == 0 {
		fmt.Println("Cannot find document")
		exitCode = 1
		return
	}
	fmt.Printf("%+v\n", findResult)

	exitCode = 0
}