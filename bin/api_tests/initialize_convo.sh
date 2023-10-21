#!/bin/bash

url="http://localhost:3000/api/initialize_convo"
payload="bin/api_tests/initialize_convo.json"

# curl a port request to url
curl -X POST -L -H "Content-Type: application/json" -d @$payload $url

