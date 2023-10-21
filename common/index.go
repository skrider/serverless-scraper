package common

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"regexp"
	"sync"

	gogpt "github.com/sashabaranov/go-gpt3"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/mat"
)

type Domain struct {
	Id     primitive.ObjectID `bson:"_id,omitempty"`
	Domain string             `bson:"domain"`
	Pages  []Page             `bson:"pages"`
}

type Page struct {
	Route    string    `bson:"route"`
	Title    string    `bson:"title"`
	Sections []Section `bson:"sections"`
}

type Conversation struct {
	Id       primitive.ObjectID `bson:"_id,omitempty"` // omitempty is actually really important here
	DomainId primitive.ObjectID `bson:"domain_id"`
	Prompt   string             `bson:"prompt"`
	Log      []string           `bson:"log"`
}

func CountPseudoTokens(str string) int {
	// count the number of words in str using regex
	re := regexp.MustCompile(`\w+`)
	return len(re.FindAllString(str, -1))
}

func (c *Conversation) AppendAgent(str string) {
	c.Log = append(c.Log, "[Agent]: "+str)
}

func (c *Conversation) AppendUser(str string) {
	c.Log = append(c.Log, "[User]: "+str)
}

func (c *Conversation) String() string {
	prompt := c.Prompt
	for _, v := range c.Log {
		prompt += "\n" + v
	}
	return prompt
}

type Section struct {
	Title     string    `bson:"title"`
	Content   string    `bson:"content"`
	Embedding []float64 `bson:"embedding"`
}

func (s *Section) Zip() string {
	return s.Title + s.Content
}

func (s *Section) Print() {
	fmt.Println("  Title: ", s.Title)
	fmt.Println("  Content: ", s.Content)
	fmt.Println("  Embedding: ", len(s.Embedding))
}

func (p *Page) Print() {
	fmt.Println("Route: ", p.Route)
	fmt.Println("Title: ", p.Title)
	for _, v := range p.Sections {
		v.Print()
	}
}

func GetDb() (*mongo.Database, func()) {
	uri := os.Getenv("MONGODB_URI")
	dbName := os.Getenv("MONGODB_DB_NAME")
	if uri == "" {
		log.Fatal("You must set your 'MONGODB_URI' environmental variable.")
	}
	if dbName == "" {
		log.Fatal("You must set your 'MONGODB_DB_NAME' environmental variable.")
	}
	client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(uri))
	db := client.Database(dbName)
	if err != nil {
		panic(err)
	}
	disconnect := func() {
		if err := client.Disconnect(context.TODO()); err != nil {
			panic(err)
		}
	}
	return db, disconnect
}

func GetAgentCompletion(c *gogpt.Client, query string) string {
	req := gogpt.CompletionRequest{
		Model:            "text-davinci-003",
		Prompt:           query,
		MaxTokens:        256,
		Temperature:      0.7,
		TopP:             1,
		PresencePenalty:  0,
		FrequencyPenalty: 0,
	}
	res, err := c.CreateCompletion(context.TODO(), req)
	if err != nil {
		panic(err.Error())
	}
	return res.Choices[0].Text
}

func (c *Conversation) ZipLog() string {
  str := ""
  for _, v := range c.Log {
    str += v + "\n"
  }
  return str
}

func hash(s string) uint32 {
  h := fnv.New32a()
  h.Write([]byte(s))
  return h.Sum32()
}

type ThreadSafeHashSet struct {
	mu    sync.Mutex
	state map[uint32]bool
}

func (c *ThreadSafeHashSet) Add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state[hash(key)] = true
}

func (c *ThreadSafeHashSet) Has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.state[hash(key)]
	return ok
}

func MakeThreadSafeHashSet() ThreadSafeHashSet {
	return ThreadSafeHashSet{
		state: map[uint32]bool{},
	}
}


const EMBEDDING_LEN = 1536
const MAX_PSEUDO_TOKENS = 1500

func GetConversationCompletion(c *gogpt.Client, conv Conversation, d Domain) string {
	chunks := make([]Section, 0)
	for _, v := range d.Pages {
		chunks = append(chunks, v.Sections...)
	}
  if len(chunks) == 0 {
    panic("no domain found or domain empty")
  }

	// data collection
	rawMatrix := make([]float64, 0, EMBEDDING_LEN*len(chunks))
	for _, v := range chunks {
		rawMatrix = append(rawMatrix, v.Embedding...)
	}
	matrix := mat.NewDense(len(chunks), EMBEDDING_LEN, rawMatrix)

  // getting embedding
  query := conv.ZipLog()
  embeddingRaw := GetEmbedding(c, query)
	embedding := mat.NewVecDense(EMBEDDING_LEN, embeddingRaw)

	// ranking
	log.Output(1, "constructing prompt")
	var dists mat.VecDense
	dists.MulVec(matrix, embedding)

	originalIndices := make([]int, len(chunks))
  floats.Scale(-1.0, dists.RawVector().Data)
	floats.Argsort(dists.RawVector().Data, originalIndices)

	// determine which docs to add
	tokens := CountPseudoTokens(query)
	indicesToAdd := make(map[int]bool, 0)
	for _, v := range originalIndices {
		tokens += CountPseudoTokens(chunks[v].Zip())
		if tokens > MAX_PSEUDO_TOKENS {
			break
		}
		indicesToAdd[v] = true
	}

	// add them back in original order
	prompt := ""
	for i, v := range chunks {
		if val, ok := indicesToAdd[i]; ok && val {
			prompt += "\n\n" + v.Zip()
		}
	}

	log.Output(1, fmt.Sprintf("composed ~%d tokens from %d subsections", tokens, len(indicesToAdd)))

	prompt += "\n\nYou are a chatbot customer support agent for a company and should continue the conversation in a cordial and professional manner using the information provided above alone to guide your responses. If you don't know the answer or the information is not provided above, refer the customer to 800-403-8023. Do not go off-topic or talk about irrelevant things--you are a customer service chatbot. Do not output an answer containing any markdown syntax."
	prompt += "\n[Agent]: Hello! What can I do for you today?"
  prompt += "\n" + query
  prompt += "\n[Agent]: "

	log.Output(1, prompt)

	log.Output(1, "requesting completion")

	// response generation
	agentResponse := GetAgentCompletion(c, prompt)

  return agentResponse
}

func GetEmbedding(c *gogpt.Client, query string) []float64 {
	embeddingReq := gogpt.EmbeddingRequest{
		Input: []string{query},
		Model: gogpt.AdaEmbeddingV2,
	}
	res, err := c.CreateEmbeddings(context.TODO(), embeddingReq)
	if err != nil {
		panic(err)
	}
	embeddingRaw := res.Data[0].Embedding
  return embeddingRaw
}
