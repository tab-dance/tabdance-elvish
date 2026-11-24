# leading comment
echo  x  # trailing
echo "# not comment"  # comment

if $true {
  echo  a
  # inside
  echo b
}
echo ^
  continued
