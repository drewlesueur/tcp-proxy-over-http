
openssl req -x509 -nodes -newkey rsa:2048 \
  -keyout key.pem -out cert.pem -days 365 \
  -config san.cnf -extensions v3_req
  
 
# Installing on macOS (Keychain Access)
# 
# To make it trusted:
# 	1.	Open Keychain Access (⌘ + Space → “Keychain Access”).
# 	2.	Select System keychain (not login).
# 	3.	Drag your cert.pem (or cert.crt) into the window.
# 	4.	Find it in the list → double-click → expand Trust.
# 	5.	Set When using this certificate → Always Trust.
# 	6.	Close → enter your password to confirm.
# 
# Now Safari, Chrome, curl, etc. will trust it.