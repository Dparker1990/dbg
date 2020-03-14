if [ $TRAVIS_OS_NAME = "linux" ] && [ $go_32_version ]; then
  docker pull i386/ubuntu:bionic
  docker help run
  docker run --env goversion=1.12 -v $(pwd):/delve i386/ubuntu:bionic /bin/bash -c "cd delve && \
  apt-get -y update && \
  apt-get -y install software-properties-common && \
  apt-get -y install git && \
  add-apt-repository ppa:longsleep/golang-backports && \
  apt-get -y install golang-$(goversion)-go && \
  export PATH=$PATH:/usr/lib/go-$(goversion)/bin && \
  go version && \
  uname -a && \
  make test"
else
  make test
fi
