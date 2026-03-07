GO_VERSION="1.23.4"

wget -N https://golang.google.cn/dl/go$GO_VERSION.linux-amd64.tar.gz
rm -rf /usr/local/go && tar -C /usr/local -xzf go$GO_VERSION.linux-amd64.tar.gz

echo "export PATH=\$PATH:/usr/local/go/bin" | tee -a /etc/profile
echo "export PATH=\$PATH:/usr/local/go/bin" | tee -a ~/.bashrc

source /etc/profile
source ~/.bashrc

go version && go env -w GO111MODULE=on && go env -w GOPROXY=https://goproxy.cn,direct

rm -f go$GO_VERSION.linux-amd64.tar.gz

echo -e "\n\033[32m✅ Go $GO_VERSION 安装成功！\033[0m"