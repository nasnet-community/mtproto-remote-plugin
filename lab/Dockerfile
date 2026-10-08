# A small Linux box for the checks a Mac cannot do: the race detector on Linux,
# and the nft capture drill (it needs nft, ip and unshare, and --privileged).
FROM golang:1.27.1-alpine
RUN apk add --no-cache nftables iproute2 util-linux-misc build-base
