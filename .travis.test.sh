if [ $TRAVIS_OS_NAME = "linux" && $go_32_version ]; then
  docker pull i386/ubuntu:bionic 
  docker run -e "go_version=$go_32_version" -v $(pwd):/delve i386/ubuntu:bionic /bin/bash -c "cd delve && \
  apt-get -y update && \
  apt-get -y install software-properties-common && \
  apt-get -y install git && \
  add-apt-repository ppa:longsleep/golang-backports && \
  apt-get -y install golang-$(go_version)-go && \
  export PATH=$PATH:/usr/lib/go-$(go_version)/bin && \
  go version && \
  uname -a && \
  make test"
else
  make test
fi
