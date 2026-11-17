// The scrubber: what removes credentials and personal data from a payload before it is sealed.
// Its own module so that a component outside the shipper can scrub with the same rules.
module github.com/QuesmaOrg/quesma-shipper/scrub

go 1.27.4
