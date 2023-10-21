#!/bin/bash

file=$1
tmpfile=$(mktemp)
# iterate through lines of file
while read -r line; do
  # skip empty lines
  [[ -z "$line" ]] && continue
  # skip comments
  [[ "$line" =~ ^#.*$ ]] && continue
  # skip lines without =
  [[ ! "$line" =~ = ]] && continue
  # split line into key and value
  key="${line%%=*}"
  value="${line#*=}"
  # export key and value
  echo "adding $key=$value"
  echo $value > $tmpfile
  yarn vercel env add "$key" development < $tmpfile
done < "$file"


