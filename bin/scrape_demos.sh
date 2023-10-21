#!/bin/bash

# create an array of URLs
URLS=("https://help.tryplayground.com/en/" "https://support.b12.io/en/" "https://www.glossier.com/" "https://numpy.org/doc/stable/reference/index.html")

payload=$(mktemp)
# loop through the URLs
for URL in "${URLS[@]}"
do
  echo "Scraping $URL"
  echo '{"domain": "'$URL'", "depth": 4}' > $payload
  curl -X POST -L -H "Content-Type: application/json" -d @$payload http://localhost:3001/scrape
done
