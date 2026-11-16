%define debug_package %{nil}

%define  uid   opencast-meet

Name:          opencast-meet
Summary:       BigBlueButton web portal with Opencast integration
Version:       %{appversion}
Release:       1
License:       MIT

Source0:       %{name}
Source1:       %{name}.service
Source2:       .env.example
Source3:       LICENSE
Source4:       README.md
URL:           https://github.com/virtuos/%{name}

BuildRequires: systemd-rpm-macros
%{?systemd_requires}


%description
A lightweight BigBlueButton web portal with Opencast integration.


%prep
cp -p %{SOURCE3} %{SOURCE4} .

%build

%install
# install binary
install -p -D -m 0755 \
   %{SOURCE0} \
   %{buildroot}%{_bindir}/%{name}

# install configuration
install -p -D -m 0640 \
   %{SOURCE2} \
   %{buildroot}%{_sysconfdir}/%{name}.conf

# install unit file
install -p -D -m 0644 \
   %{SOURCE1} \
   %{buildroot}%{_unitdir}/%{name}.service


%pre
# Create user and group if they don't exist
if [ ! $(getent passwd %{uid}) ]; then
   useradd --system --user-group %{uid} > /dev/null 2>&1 || :
fi


%post
%systemd_post %{name}.service


%preun
%systemd_preun %{name}.service


%postun
%systemd_postun_with_restart %{name}.service


%files
%{_bindir}/%{name}
%config(noreplace) %attr(640,root,%{uid}) %{_sysconfdir}/%{name}.conf
%{_unitdir}/%{name}.service
%license LICENSE
%doc README.md


%changelog
* Thu Jun 18 2026 Lars Kiesow <lkiesow@uos.de> - 0.1.0-1
- Initial build
