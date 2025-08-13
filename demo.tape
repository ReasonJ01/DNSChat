Output demo.gif

Require echo

Set Shell "bash"
Set FontSize 24
Set Width 1200
Set Height 600
Set Theme "GruvboxDark"
Set WindowBar Colorful
Set Margin 15
Set MarginFill "#1a592b"
Set BorderRadius 10

# Hide setup
Hide
Type 'dig() { command dig @127.0.0.1 -p 8081 "$@" +short +time=10 | fold -s -w 65; }'
Enter
Type clear
Enter
Show


# Now the visible command has no flags, but they’re applied by the alias
Type `dig @127.0.0.1 "Greet LinkedIn, then explain what DNS is" TXT`
Enter
Wait
Sleep 2s
Type `dig @127.0.0.1 "Is it kinda unusual to be able to chat with an LLM over DNS?" TXT`
Enter
Wait
Sleep 5s