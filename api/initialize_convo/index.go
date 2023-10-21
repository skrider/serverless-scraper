package initialize_convo_go

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/passage-inc/chatassist/packages/vercel/common"
	gogpt "github.com/sashabaranov/go-gpt3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type postRequest struct {
	Domain   string `json:"domain"`
	Question string `json:"question"`
}

type postResponse struct {
	Answer         string             `json:"answer"`
	ConversationId primitive.ObjectID `json:"conversation_id"`
	Error          string             `json:"error,omitempty"`
	Success        bool               `json:"success"`
}

func handlePost(w *http.ResponseWriter, r *http.Request) {
	c := gogpt.NewClient(os.Getenv("OPENAI_API_KEY"))
	ctx := context.TODO()
	db, disconnect := common.GetDb()
	defer disconnect()

	var req postRequest
	// parse from request body
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		panic(err.Error())
	}

  parsed, err := url.Parse(req.Domain)
  if err != nil {
    panic(err)
  }
  decodedDomain := parsed.Host + parsed.Path
	log.Output(1, "finding domain"+decodedDomain)

	item := db.Collection("ScrapedDomains").FindOne(ctx, bson.D{{"domain", decodedDomain}})
	var targetDomain common.Domain
	item.Decode(&targetDomain)

	convo := common.Conversation{
		DomainId: targetDomain.Id,
		Log:      []string{},
	}
  convo.AppendUser(req.Question)

	log.Output(1, "constructing key matrix")

	agentResponse := common.GetConversationCompletion(c, convo, targetDomain)
  convo.AppendAgent(agentResponse)

	log.Output(1, "uploading conversation")

	// save convo to database
	res3, err := db.Collection("Conversations").InsertOne(context.TODO(), convo)
	if err != nil {
		panic(err.Error())
	}

	response := postResponse{
		Answer:         agentResponse,
		ConversationId: res3.InsertedID.(primitive.ObjectID),
		Success:        true,
	}

	log.Output(1, "done")

	// send the response
	(*w).Header().Set("Content-Type", "application/json")
	json.NewEncoder(*w).Encode(response)
}

func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == "POST" {
		handlePost(&w, r)
	}
}
