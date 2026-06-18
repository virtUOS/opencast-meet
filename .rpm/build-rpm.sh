#!/bin/bash

set -eux

dnf install -y rpmdevtools
rpmdev-setuptree

echo "%appversion ${2}" >>~/.rpmmacros
cp "/rpmbuild/${1}" ~/rpmbuild/SOURCES/opencast-meet
cp /rpmbuild/.env.example ~/rpmbuild/SOURCES/
cp /rpmbuild/.rpm/opencast-meet.service ~/rpmbuild/SOURCES/
cp /rpmbuild/.rpm/opencast-meet.spec ~/rpmbuild/SPECS/
cp /rpmbuild/LICENSE ~/rpmbuild/BUILD/
cp /rpmbuild/README.md ~/rpmbuild/BUILD/

rpmbuild -ba --target "${3}" ~/rpmbuild/SPECS/opencast-meet.spec

cp ~/rpmbuild/RPMS/${3}/opencast-meet*.rpm /rpmbuild/
