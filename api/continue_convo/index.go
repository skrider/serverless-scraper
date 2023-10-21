package continue_convo_go

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/passage-inc/chatassist/packages/vercel/common"
	gogpt "github.com/sashabaranov/go-gpt3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type postRequest struct {
	ConversationId primitive.ObjectID `json:"conversation_id" bson:"conversation_id"`
	Message        string             `json:"message" bson:"message"`
}

type postResponse struct {
	Response string `json:"response"`
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

	log.Output(1, "finding convo with id"+req.ConversationId.String())

	// find conversation with matching ID
	var convo common.Conversation
	db.Collection("Conversations").FindOne(ctx, bson.D{{"_id", req.ConversationId}}).Decode(&convo)

	// find the domain with the correct domainId
	var domain common.Domain
	db.Collection("ScrapedDomains").FindOne(ctx, bson.D{{"_id", convo.DomainId}}).Decode(&domain)

	convo.AppendUser(req.Message)
	agentResponse := common.GetConversationCompletion(c, convo, domain)
	convo.AppendAgent(agentResponse)

	log.Output(1, "persisting conversation")

	// update convo in database
	_, err = db.Collection("Conversations").UpdateOne(ctx, bson.M{"_id": req.ConversationId}, bson.M{"$set": bson.M{"log": convo.Log}})
	if err != nil {
		panic(err.Error())
	}

	log.Output(1, "responding")
	response := postResponse{
		Response: agentResponse,
	}

	(*w).Header().Set("Content-Type", "application/json")
	json.NewEncoder(*w).Encode(response)
}

func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == "POST" {
		handlePost(&w, r)
	}
}
